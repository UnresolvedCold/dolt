// Copyright 2021 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package creation

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/dolthub/go-mysql-server/sql"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb/durable"
	"github.com/dolthub/dolt/go/libraries/doltcore/schema"
	"github.com/dolthub/dolt/go/libraries/doltcore/table/editor"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/shim"
	"github.com/dolthub/dolt/go/store/types"
	"github.com/dolthub/dolt/go/store/val"
)

type CreateIndexReturn struct {
	NewTable *doltdb.Table
	Sch      schema.Schema
	OldIndex schema.Index
	NewIndex schema.Index
}

// CreateIndex creates the given index on the given table with the given schema. Returns the updated table, updated schema, and created index.
func CreateIndex(
	ctx context.Context,
	table *doltdb.Table,
	indexName string,
	columns []string,
	isUnique bool,
	isUserDefined bool,
	comment string,
	opts editor.Options,
) (*CreateIndexReturn, error) {
	sch, err := table.GetSchema(ctx)
	if err != nil {
		return nil, err
	}

	// get the real column names as CREATE INDEX columns are case-insensitive
	var realColNames []string
	allTableCols := sch.GetAllCols()
	for _, indexCol := range columns {
		tableCol, ok := allTableCols.GetByNameCaseInsensitive(indexCol)
		if !ok {
			return nil, fmt.Errorf("column `%s` does not exist for the table", indexCol)
		}
		realColNames = append(realColNames, tableCol.Name)
	}

	if indexName == "" {
		indexName = strings.Join(realColNames, "")
		_, ok := sch.Indexes().GetByNameCaseInsensitive(indexName)
		var i int
		for ok {
			i++
			indexName = fmt.Sprintf("%s_%d", strings.Join(realColNames, ""), i)
			_, ok = sch.Indexes().GetByNameCaseInsensitive(indexName)
		}
	}
	if !doltdb.IsValidIndexName(indexName) {
		return nil, fmt.Errorf("invalid index name `%s` as they must match the regular expression %s", indexName, doltdb.IndexNameRegexStr)
	}

	// if an index was already created for the column set but was not generated by the user then we replace it
	existingIndex, ok := sch.Indexes().GetIndexByColumnNames(realColNames...)
	if ok && !existingIndex.IsUserDefined() {
		_, err = sch.Indexes().RemoveIndex(existingIndex.Name())
		if err != nil {
			return nil, err
		}
		table, err = table.DeleteIndexRowData(ctx, existingIndex.Name())
		if err != nil {
			return nil, err
		}
	}

	// create the index metadata, will error if index names are taken or an index with the same columns in the same order exists
	index, err := sch.Indexes().AddIndexByColNames(
		indexName,
		realColNames,
		schema.IndexProperties{
			IsUnique:      isUnique,
			IsUserDefined: isUserDefined,
			Comment:       comment,
		},
	)
	if err != nil {
		return nil, err
	}

	// update the table schema with the new index
	newTable, err := table.UpdateSchema(ctx, sch)
	if err != nil {
		return nil, err
	}

	// TODO: in the case that we're replacing an implicit index with one the user specified, we could do this more
	//  cheaply in some cases by just renaming it, rather than building it from scratch. But that's harder to get right.
	indexRows, err := BuildSecondaryIndex(ctx, newTable, index, opts)
	if err != nil {
		return nil, err
	}

	newTable, err = newTable.SetIndexRows(ctx, index.Name(), indexRows)
	if err != nil {
		return nil, err
	}

	return &CreateIndexReturn{
		NewTable: newTable,
		Sch:      sch,
		OldIndex: existingIndex,
		NewIndex: index,
	}, nil
}

func BuildSecondaryIndex(ctx context.Context, tbl *doltdb.Table, idx schema.Index, opts editor.Options) (durable.Index, error) {
	switch tbl.Format() {
	case types.Format_LD_1, types.Format_DOLT_DEV:
		m, err := editor.RebuildIndex(ctx, tbl, idx.Name(), opts)
		if err != nil {
			return nil, err
		}
		return durable.IndexFromNomsMap(m, tbl.ValueReadWriter()), nil

	case types.Format_DOLT_1:
		sch, err := tbl.GetSchema(ctx)
		if err != nil {
			return nil, err
		}
		m, err := tbl.GetRowData(ctx)
		if err != nil {
			return nil, err
		}
		primary := durable.ProllyMapFromIndex(m)
		return BuildSecondaryProllyIndex(ctx, tbl.ValueReadWriter(), sch, idx, primary)

	default:
		return nil, fmt.Errorf("unknown NomsBinFormat")
	}
}

// BuildSecondaryProllyIndex builds secondary index data for the given primary
// index row data |primary|. |sch| is the current schema of the table.
func BuildSecondaryProllyIndex(ctx context.Context, vrw types.ValueReadWriter, sch schema.Schema, idx schema.Index, primary prolly.Map) (durable.Index, error) {
	if idx.IsUnique() {
		kd := shim.KeyDescriptorFromSchema(idx.Schema())
		return BuildUniqueProllyIndex(ctx, vrw, sch, idx, primary, func(ctx context.Context, existingKey, newKey val.Tuple) error {
			return sql.ErrDuplicateEntry.Wrap(&prollyUniqueKeyErr{k: newKey, kd: kd, IndexName: idx.Name()}, idx.Name())
		})
	}

	empty, err := durable.NewEmptyIndex(ctx, vrw, idx.Schema())
	if err != nil {
		return nil, err
	}
	secondary := durable.ProllyMapFromIndex(empty)

	iter, err := primary.IterAll(ctx)
	if err != nil {
		return nil, err
	}
	pkLen := sch.GetPKCols().Size()

	// create a key builder for index key tuples
	kd, _ := secondary.Descriptors()
	keyBld := val.NewTupleBuilder(kd)
	keyMap := GetIndexKeyMapping(sch, idx)

	mut := secondary.Mutate()
	for {
		k, v, err := iter.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		for to := range keyMap {
			from := keyMap.MapOrdinal(to)
			if from < pkLen {
				keyBld.PutRaw(to, k.GetField(from))
			} else {
				from -= pkLen
				keyBld.PutRaw(to, v.GetField(from))
			}
		}

		// todo(andy): build permissive?
		idxKey := keyBld.Build(primary.Pool())
		idxVal := val.EmptyTuple

		// todo(andy): periodic flushing
		if err = mut.Put(ctx, idxKey, idxVal); err != nil {
			return nil, err
		}
	}

	secondary, err = mut.Map(ctx)
	if err != nil {
		return nil, err
	}

	return durable.IndexFromProllyMap(secondary), nil
}

// DupEntryCb receives duplicate unique index entries.
type DupEntryCb func(ctx context.Context, existingKey, newKey val.Tuple) error

// BuildUniqueProllyIndex builds a unique index based on the given |primary| row
// data. If any duplicate entries are found, they are passed to |cb|. If |cb|
// returns a non-nil error then the process is stopped.
func BuildUniqueProllyIndex(ctx context.Context, vrw types.ValueReadWriter, sch schema.Schema, idx schema.Index, primary prolly.Map, cb DupEntryCb) (durable.Index, error) {
	empty, err := durable.NewEmptyIndex(ctx, vrw, idx.Schema())
	if err != nil {
		return nil, err
	}
	secondary := durable.ProllyMapFromIndex(empty)

	iter, err := primary.IterAll(ctx)
	if err != nil {
		return nil, err
	}
	pkLen := sch.GetPKCols().Size()

	// create a key builder for index key tuples
	kd, _ := secondary.Descriptors()
	keyBld := val.NewTupleBuilder(kd)
	keyMap := GetIndexKeyMapping(sch, idx)

	// key builder for the indexed columns only which is a prefix of the index key
	prefixKD := kd.PrefixDesc(idx.Count())
	prefixKB := val.NewTupleBuilder(prefixKD)

	p := primary.Pool()

	mut := secondary.Mutate()
	for {
		k, v, err := iter.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		foundNullPrefix := false
		prefixKB.Recycle()
		for to := range keyMap {
			from := keyMap.MapOrdinal(to)
			var f []byte
			if from < pkLen {
				f = k.GetField(from)
			} else {
				from -= pkLen
				f = v.GetField(from)
			}
			keyBld.PutRaw(to, f)
			if to < prefixKD.Count() {
				if f == nil {
					foundNullPrefix = true
				} else {
					prefixKB.PutRaw(to, f)
				}
			}
		}

		idxKey := keyBld.Build(p)
		idxVal := val.EmptyTuple

		if !foundNullPrefix {
			prefixKey := prefixKB.Build(p)

			itr, err := NewPrefixItr(ctx, prefixKey, prefixKD, mut)
			if err != nil {
				return nil, err
			}

			k, _, err = itr.Next(ctx)
			if err != nil && err != io.EOF {
				return nil, err
			}
			if err == nil {
				// We found a duplicate entry so delegate behavior to callback.
				if err = cb(ctx, k, idxKey); err != nil {
					return nil, err
				}
			}
		}

		if err = mut.Put(ctx, idxKey, idxVal); err != nil {
			return nil, err
		}
	}

	secondary, err = mut.Map(ctx)
	if err != nil {
		return nil, err
	}

	return durable.IndexFromProllyMap(secondary), nil
}

// PrefixItr iterates all keys of a given prefix |p| and its descriptor |d| in
// map |m|.
type PrefixItr struct {
	itr prolly.MapIter
	p   val.Tuple
	d   val.TupleDesc
}

func NewPrefixItr(ctx context.Context, p val.Tuple, d val.TupleDesc, m rangeIterator) (PrefixItr, error) {
	rng := prolly.ClosedRange(p, p, d)
	itr, err := m.IterRange(ctx, rng)
	if err != nil {
		return PrefixItr{}, err
	}
	return PrefixItr{p: p, d: d, itr: itr}, nil
}

func (itr PrefixItr) Next(ctx context.Context) (k, v val.Tuple, err error) {
OUTER:
	for {
		k, v, err = itr.itr.Next(ctx)
		if err != nil {
			return nil, nil, err
		}

		// check if p is a prefix of k
		// range iteration currently can return keys not in the range
		for i := 0; i < itr.p.Count(); i++ {
			f1 := itr.p.GetField(i)
			f2 := k.GetField(i)
			if bytes.Compare(f1, f2) != 0 {
				// if a field in the prefix does not match |k|, go to the next row
				continue OUTER
			}
		}

		return k, v, nil
	}
}

type rangeIterator interface {
	IterRange(ctx context.Context, rng prolly.Range) (prolly.MapIter, error)
}

func GetIndexKeyMapping(sch schema.Schema, idx schema.Index) (m val.OrdinalMapping) {
	m = make(val.OrdinalMapping, len(idx.AllTags()))

	for i, tag := range idx.AllTags() {
		j, ok := sch.GetPKCols().TagToIdx[tag]
		if !ok {
			j = sch.GetNonPKCols().TagToIdx[tag]
			j += sch.GetPKCols().Size()
		}
		m[i] = j
	}

	return
}

var _ error = (*prollyUniqueKeyErr)(nil)

// prollyUniqueKeyErr is an error that is returned when a unique constraint has been violated. It contains the index key
// (which is the full row).
type prollyUniqueKeyErr struct {
	k         val.Tuple
	kd        val.TupleDesc
	IndexName string
}

// Error implements the error interface.
func (u *prollyUniqueKeyErr) Error() string {
	keyStr, _ := formatKey(u.k, u.kd)
	return fmt.Sprintf("duplicate unique key given: %s", keyStr)
}

// formatKey returns a comma-separated string representation of the key given
// that matches the output of the old format.
func formatKey(key val.Tuple, td val.TupleDesc) (string, error) {
	vals := make([]string, td.Count())
	for i := 0; i < td.Count(); i++ {
		vals[i] = td.FormatValue(i, key.GetField(i))
	}

	return fmt.Sprintf("[%s]", strings.Join(vals, ",")), nil
}
