// Package internal contains reflection helpers used by dbt.
//
// All struct inspection is done on reflect.Type and cached, so nothing here
// needs a "zero value" instance of the struct (the previous Make/StructFields
// design allocated a fresh value tree on every call).
package internal

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/alextanhongpin/core/types/stringcase"
)

var (
	timeType    = reflect.TypeFor[time.Time]()
	scannerType = reflect.TypeFor[sql.Scanner]()
	valuerType  = reflect.TypeFor[driver.Valuer]()

	// Tag keys, in lookup order.
	tagKeys = []string{"db", "json"}
)

// Field describes a single column-backed struct field.
type Field struct {
	// Name is the logical, possibly dotted, column name, e.g. "author.name".
	// Dotted names come from inlined/embedded structs and are treated as
	// "<table alias>.<column>" when selecting.
	Name string
	// Alias is Name with dots replaced by underscores. It is both the
	// result-set column name (select "author"."name" as "author_name") and the
	// @param name.
	Alias string
	// Index is the path for reflect.Value.Field, through embedded structs.
	Index []int
}

// Get returns the field value from the struct value v. If the path crosses a
// nil embedded pointer, it returns nil (SQL NULL). v must be a struct.
func (f Field) Get(v reflect.Value) any {
	for n, i := range f.Index {
		if n > 0 {
			for v.Kind() == reflect.Pointer {
				if v.IsNil() {
					return nil
				}
				v = v.Elem()
			}
		}
		v = v.Field(i)
	}
	return v.Interface()
}

// Ptr returns a pointer to the field inside v for scanning, allocating nil
// embedded pointers on the way. v must be an addressable struct.
func (f Field) Ptr(v reflect.Value) any {
	for n, i := range f.Index {
		if n > 0 {
			for v.Kind() == reflect.Pointer {
				if v.IsNil() {
					v.Set(reflect.New(v.Type().Elem()))
				}
				v = v.Elem()
			}
		}
		v = v.Field(i)
	}
	return v.Addr().Interface()
}

// IsLeaf reports whether t should be treated as a single column value rather
// than something to flatten: every non-struct, time.Time, and anything that
// implements sql.Scanner or driver.Valuer (sql.NullString, uuid.UUID, ...).
func IsLeaf(t reflect.Type) bool {
	if t.Kind() != reflect.Struct {
		return true
	}
	if t == timeType {
		return true
	}
	pt := reflect.PointerTo(t)
	return pt.Implements(scannerType) || pt.Implements(valuerType) || t.Implements(valuerType)
}

type cacheEntry struct {
	fields []Field
	err    error
}

var cache sync.Map // reflect.Type -> cacheEntry

// FieldsOf returns the column fields of struct type t (one pointer level is
// dereferenced). It returns nil for leaf types, e.g. int64 or time.Time.
// Results are cached per type.
func FieldsOf(t reflect.Type) ([]Field, error) {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if IsLeaf(t) {
		return nil, nil
	}
	if e, ok := cache.Load(t); ok {
		ce := e.(cacheEntry)
		return ce.fields, ce.err
	}

	fields, err := collect(t, nil, "", map[reflect.Type]bool{t: true})
	if err == nil {
		err = checkDuplicates(t, fields)
	}
	cache.Store(t, cacheEntry{fields, err})
	return fields, err
}

func checkDuplicates(t reflect.Type, fields []Field) error {
	seen := make(map[string]string, len(fields))
	for _, f := range fields {
		if prev, ok := seen[f.Alias]; ok {
			return fmt.Errorf("dbt: %s: columns %q and %q both map to %q", t, prev, f.Name, f.Alias)
		}
		seen[f.Alias] = f.Name
	}
	return nil
}

type tagInfo struct {
	name    string
	skip    bool
	inline  bool // recurse, prefixing nested names with this field's name
	flatten bool // recurse, no prefix (e.g. a shared base model)
}

func parseTag(sf reflect.StructField) tagInfo {
	for _, key := range tagKeys {
		v, ok := sf.Tag.Lookup(key)
		if !ok {
			continue
		}
		if v == "-" {
			return tagInfo{skip: true}
		}
		parts := strings.Split(v, ",")
		info := tagInfo{name: parts[0]}
		for _, opt := range parts[1:] {
			switch opt {
			case "inline":
				info.inline = true
			case "flatten":
				info.flatten = true
			}
		}
		return info
	}
	return tagInfo{}
}

func collect(t reflect.Type, index []int, prefix string, seen map[reflect.Type]bool) ([]Field, error) {
	var out []Field
	for i := range t.NumField() {
		sf := t.Field(i)
		tag := parseTag(sf)
		if tag.skip {
			continue
		}

		ft := sf.Type
		ptr := ft.Kind() == reflect.Pointer
		bt := ft
		if ptr {
			bt = ft.Elem()
		}

		if !sf.IsExported() {
			// Unexported embedded structs may still expose exported fields,
			// but an unexported embedded *pointer* can't be allocated.
			if !sf.Anonymous || ptr {
				continue
			}
		}

		name := tag.name
		if name == "" {
			name = stringcase.ToSnake(sf.Name)
		}

		explicit := tag.inline || tag.flatten
		canInline := bt.Kind() == reflect.Struct && !IsLeaf(bt)
		if explicit && !canInline {
			return nil, fmt.Errorf("dbt: %s.%s: inline/flatten requires a plain struct, got %s", t, sf.Name, ft)
		}

		path := append(slices.Clone(index), i)

		if canInline && (explicit || sf.Anonymous) {
			if seen[bt] {
				return nil, fmt.Errorf("dbt: %s: recursive embedding of %s", t, bt)
			}
			nestedPrefix := prefix
			if !tag.flatten {
				nestedPrefix = joinName(prefix, name)
			}
			seen[bt] = true
			nested, err := collect(bt, path, nestedPrefix, seen)
			delete(seen, bt)
			if err != nil {
				return nil, err
			}
			out = append(out, nested...)
			continue
		}

		if !sf.IsExported() {
			continue
		}
		full := joinName(prefix, name)
		out = append(out, Field{
			Name:  full,
			Alias: strings.ReplaceAll(full, ".", "_"),
			Index: path,
		})
	}
	return out, nil
}

func joinName(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}
