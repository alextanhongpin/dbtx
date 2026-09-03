package internal

import (
	"cmp"
	"reflect"
	"strings"

	"github.com/alextanhongpin/core/types/stringcase"
)

type StructFieldInfo struct {
	Name  string
	Value reflect.Value
}

type StructFieldConfig struct {
	Tag string
}

type StructFieldOptions func(*StructFieldConfig)

func StructFieldsType[T any](opts ...StructFieldOptions) []StructFieldInfo {
	v := Make[T]()
	return StructFields(v, opts...)
}

func StructFields(a any, opts ...StructFieldOptions) []StructFieldInfo {
	cfg := StructFieldConfig{
		Tag: "json",
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	v := reflect.ValueOf(a)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil
	}
	return structFieldsWithPrefix(v, cfg.Tag, "")
}

func structFieldsWithPrefix(v reflect.Value, tag, prefix string) []StructFieldInfo {
	var out []StructFieldInfo
	t := v.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		fv := v.Field(i)
		tag := f.Tag.Get(tag)
		if tag == "-" {
			continue
		}
		name, inline := strings.CutSuffix(tag, ",inline")
		if name == "" {
			name = stringcase.ToSnake(f.Name)
		}
		inline = cmp.Or(inline, f.Anonymous)
		fullName := joinName(prefix, name)
		if inline {
			if fv.Kind() == reflect.Pointer {
				fv = fv.Elem()
			}
			nested := structFieldsWithPrefix(fv, tag, fullName)
			out = append(out, nested...)
		} else {
			out = append(out, StructFieldInfo{
				Name:  fullName,
				Value: fv,
			})
		}
	}
	return out
}

func joinName(prefix, name string) string {
	if prefix == "" {
		return name
	}
	if name == "" {
		return prefix
	}
	return prefix + "." + name
}
