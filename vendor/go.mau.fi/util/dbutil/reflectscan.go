// Copyright (c) 2026 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package dbutil

import (
	"fmt"
	"reflect"
)

// MakeSimpleReflectScanner creates a ConvertRowFn that uses reflection to scan rows into the given type.
//
// This is a simplified implementation that always scans to all struct fields. It does not support any kind of struct tags.
func MakeSimpleReflectScanner[T any]() ConvertRowFn[*T] {
	fields := reflect.VisibleFields(reflect.TypeFor[T]())
	return func(row Scannable) (*T, error) {
		t := new(T)
		val := reflect.ValueOf(t).Elem()
		scanInto := make([]any, len(fields))
		for i, field := range fields {
			scanInto[i] = val.FieldByIndex(field.Index).Addr().Interface()
		}
		err := row.Scan(scanInto...)
		return t, err
	}
}

func getFieldMap[T any](structTag string) map[string][]int {
	fields := reflect.VisibleFields(reflect.TypeFor[T]())
	m := make(map[string][]int, len(fields))
	for _, field := range fields {
		sqlName := field.Tag.Get(structTag)
		if sqlName == "" {
			sqlName = field.Name
		}
		m[sqlName] = field.Index
	}
	return m
}

const defaultReflectStructTag = "column"

type ReflectScanOptions struct {
	StructTag     string
	IgnoreUnknown bool
}

type noopScan struct{}

func (*noopScan) Scan(_ any) error {
	return nil
}

var noopScanVal = &noopScan{}

func initReflectScanWithRows[T any](rows Rows, opts ReflectScanOptions) ([][]int, error) {
	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("reflectscan: failed to get columns: %w", err)
	}
	return initReflectScanWithColumns[T](columns, opts)
}

func initReflectScanWithColumns[T any](columns []string, opts ReflectScanOptions) ([][]int, error) {
	fieldMap := getFieldMap[T](opts.StructTag)
	fields := make([][]int, len(columns))
	var ok bool
	for i, col := range columns {
		fields[i], ok = fieldMap[col]
		if !ok && !opts.IgnoreUnknown {
			return nil, fmt.Errorf("reflectscan: column %q does not match any struct field", col)
		}
	}
	return fields, nil
}

func makeReflectScannerWithFields[T any](fields [][]int) ConvertRowFn[*T] {
	return func(row Scannable) (*T, error) {
		t := new(T)
		val := reflect.ValueOf(t).Elem()
		scanInto := make([]any, len(fields))
		for i, idx := range fields {
			if idx == nil {
				scanInto[i] = noopScanVal
			} else {
				scanInto[i] = val.FieldByIndex(idx).Addr().Interface()
			}
		}
		err := row.Scan(scanInto...)
		return t, err
	}
}

// MakeReflectScanner creates a ConvertRowFn that uses reflection to scan rows into the given type.
//
// To detect column names automatically, use [NewReflectRowIter] instead.
func MakeReflectScanner[T any](columns []string, opts ...ReflectScanOptions) (ConvertRowFn[*T], error) {
	var opt ReflectScanOptions
	if len(opts) == 1 {
		opt = opts[0]
	} else if len(opts) > 1 {
		return nil, fmt.Errorf("reflectscan: only one ReflectScanOptions is allowed")
	}
	fields, err := initReflectScanWithColumns[T](columns, opt)
	if err != nil {
		return nil, err
	}
	return makeReflectScannerWithFields[T](fields), nil
}

func makeReflectScanner[T any](rows Rows, err error, opts ReflectScanOptions) (ConvertRowFn[*T], error) {
	if err != nil {
		return nil, err
	}
	fields, err := initReflectScanWithRows[T](rows, opts)
	if err != nil {
		return nil, err
	}
	return makeReflectScannerWithFields[T](fields), nil
}

// NewSimpleReflectRowIter creates a new RowIter that uses reflection to scan rows into the given type.
//
// This is a simplified implementation that always scans to all struct fields. It does not support any kind of struct tags.
func NewSimpleReflectRowIter[T any](rows Rows, err error) RowIter[*T] {
	return MakeSimpleReflectScanner[T]().NewRowIter(rows, err)
}

// NewReflectRowIter creates a new RowIter that uses reflection to scan rows into the given type.
//
// This will use the `column` struct tag. The column names returned by the db must match an explicit struct tag exactly.
// Use [NewReflectRowIterWithOptions] to customize the struct tag or ignore unknown columns.
func NewReflectRowIter[T any](rows Rows, err error) RowIter[*T] {
	return NewReflectRowIterWithOptions[T](rows, err, ReflectScanOptions{StructTag: defaultReflectStructTag})
}

func NewReflectRowIterWithOptions[T any](rows Rows, err error, opts ReflectScanOptions) RowIter[*T] {
	fn, err := makeReflectScanner[T](rows, err, opts)
	return fn.NewRowIter(rows, err)
}
