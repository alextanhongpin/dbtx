package internal

import (
	"reflect"
)

func IsPointerType[T any]() bool {
	return reflect.TypeFor[T]().Kind() == reflect.Pointer
}

// Make returns a non-nil struct of type T, with all pointer fields initialized
// to be non-nil.
func Make[T any]() T {
	v := reflect.ValueOf(*new(T))
	// Handles "any".
	if !v.IsValid() {
		var zero T
		return zero
	}
	return valueFor(v).Interface().(T)
}

func setStructFields(val reflect.Value) {
	if val.Kind() == reflect.Pointer {
		val = val.Elem()
	}
	if val.Kind() != reflect.Struct {
		return
	}
	for _, v := range val.Fields() {
		// Recursively check for field pointers, and set it to non-nil.
		if v.CanSet() && v.Kind() == reflect.Pointer {
			v.Set(valueFor(v))
		}
	}

}
func valueFor(val reflect.Value) reflect.Value {
	switch val.Kind() {
	case reflect.Pointer:
		val = reflect.New(val.Type().Elem())
	default:
		val = reflect.New(val.Type()).Elem()
	}
	setStructFields(val)
	return val
}
