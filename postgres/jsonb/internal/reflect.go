package internal

import (
	"fmt"
	"reflect"

	"github.com/alextanhongpin/core/types/stringcase"
)

// Difference stores the results of the comparison
type Difference struct {
	MissingInMap []string // Present in Struct, missing in Map
	ExtraInMap   []string // Present in Map, missing in Struct
}

func (d Difference) Different() bool {
	return len(d.MissingInMap)+len(d.ExtraInMap) > 0
}

// CompareStructAndMap initiates the recursive comparison at either struct or slice root level
func CompareStructAndMap(s any, m any) Difference {
	diff := Difference{
		MissingInMap: make([]string, 0),
		ExtraInMap:   make([]string, 0),
	}

	v := reflect.ValueOf(s)
	// Dereference pointer if the root argument is a pointer to a struct/slice
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return diff
		}
		v = v.Elem()
	}

	switch v.Kind() {
	case reflect.Struct:
		if targetMap, ok := m.(map[string]any); ok {
			compareRecursive(v, targetMap, "", &diff)
		} else {
			// If target map is missing completely for a struct
			diff.MissingInMap = append(diff.MissingInMap, "(root struct missing in map)")
		}
	case reflect.Slice:
		if targetSlice, ok := m.([]any); ok {
			compareSlice(v, targetSlice, "", &diff)
		} else {
			// If target slice is missing completely for a slice
			diff.MissingInMap = append(diff.MissingInMap, "(root slice missing in map)")
		}
	}

	return diff
}

func compareRecursive(v reflect.Value, m map[string]any, prefix string, diff *Difference) {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}

	if v.Kind() != reflect.Struct {
		return
	}

	t := v.Type()
	structFields := make(map[string]reflect.Value)

	// 1. Collect all fields from the struct
	for i := 0; i < v.NumField(); i++ {
		fieldT := t.Field(i)
		fieldV := v.Field(i)

		if !fieldT.IsExported() {
			continue
		}

		// Use json tag if available, fallback to field Name
		name := fieldT.Tag.Get("json")
		if name == "" || name == "-" {
			name = stringcase.ToSnake(fieldT.Name)
		} else {
			// Handle tags like "name,omitempty"
			for idx, ch := range name {
				if ch == ',' {
					name = name[:idx]
					break
				}
			}
		}

		structFields[name] = fieldV

		// Check if the expected struct field is missing in the map
		mapVal, exists := m[name]
		fullPath := appendPath(prefix, name)
		if !exists {
			diff.MissingInMap = append(diff.MissingInMap, fullPath)
			continue
		}

		nestedMap, isMap := mapVal.(map[string]any)
		nestedMapSlice, isMapSlice := mapVal.([]any)

		deepV := fieldV
		if deepV.Kind() == reflect.Pointer && !deepV.IsNil() {
			deepV = deepV.Elem()
		}

		switch deepV.Kind() {
		case reflect.Struct:
			if isMap {
				compareRecursive(deepV, nestedMap, fullPath, diff)
			}
		case reflect.Slice:
			if isMapSlice {
				compareSlice(deepV, nestedMapSlice, fullPath, diff)
			}
		}
	}

	// 2. Check for extra keys in the map that aren't in the struct
	for k := range m {
		if _, exists := structFields[k]; !exists {
			diff.ExtraInMap = append(diff.ExtraInMap, appendPath(prefix, k))
		}
	}
}

func compareSlice(v reflect.Value, slice []any, prefix string, diff *Difference) {
	for i := 0; i < v.Len(); i++ {
		elemV := v.Index(i)
		if elemV.Kind() == reflect.Pointer && !elemV.IsNil() {
			elemV = elemV.Elem()
		}

		// Construct index path element notation (e.g. "[0]" or "users[0]")
		var indexPath string
		if prefix == "" {
			indexPath = fmt.Sprintf("[%d]", i)
		} else {
			indexPath = fmt.Sprintf("%s[%d]", prefix, i)
		}

		if elemV.Kind() == reflect.Struct && i < len(slice) {
			if nestedMap, ok := slice[i].(map[string]any); ok {
				compareRecursive(elemV, nestedMap, indexPath, diff)
			}
		}
	}

	// Optional: Check if map slice has more elements than the struct slice
	if len(slice) > v.Len() && v.Len() > 0 {
		// Sample the structure profile from the first element of the struct slice to find extra keys in downstream elements
		sampleElem := v.Index(0)
		if sampleElem.Kind() == reflect.Pointer && !sampleElem.IsNil() {
			sampleElem = sampleElem.Elem()
		}

		if sampleElem.Kind() == reflect.Struct {
			for i := v.Len(); i < len(slice); i++ {
				var indexPath string
				if prefix == "" {
					indexPath = fmt.Sprintf("[%d]", i)
				} else {
					indexPath = fmt.Sprintf("%s[%d]", prefix, i)
				}
				if extraMap, ok := slice[i].(map[string]any); ok {
					// Passing empty struct logic forces all elements inside this extra map to be registered as ExtraInMap
					compareRecursive(reflect.ValueOf(struct{}{}), extraMap, indexPath, diff)
				}
			}
		}
	}
}

func appendPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}
