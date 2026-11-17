package hue

import "math"

// The bridge's JSON is read loosely: a missing or mistyped field means "not
// there", never an error. These helpers walk decoded JSON (map[string]any,
// []any, float64, string, bool) along a path of object keys.

func at(v any, path ...string) (any, bool) {
	for _, key := range path {
		object, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		v, ok = object[key]
		if !ok {
			return nil, false
		}
	}
	return v, true
}

func stringAt(v any, path ...string) (string, bool) {
	value, ok := at(v, path...)
	if !ok {
		return "", false
	}
	s, ok := value.(string)
	return s, ok
}

func boolAt(v any, path ...string) (bool, bool) {
	value, ok := at(v, path...)
	if !ok {
		return false, false
	}
	b, ok := value.(bool)
	return b, ok
}

func floatAt(v any, path ...string) (float64, bool) {
	value, ok := at(v, path...)
	if !ok {
		return 0, false
	}
	f, ok := value.(float64)
	return f, ok
}

// uint16At reads a whole, non-negative number that fits in 16 bits.
func uint16At(v any, path ...string) (uint16, bool) {
	f, ok := floatAt(v, path...)
	if !ok || f < 0 || f > math.MaxUint16 || f != math.Trunc(f) {
		return 0, false
	}
	return uint16(f), true
}

func arrayAt(v any, path ...string) []any {
	value, _ := at(v, path...)
	array, _ := value.([]any)
	return array
}

func isObjectAt(v any, path ...string) bool {
	value, ok := at(v, path...)
	if !ok {
		return false
	}
	_, isObject := value.(map[string]any)
	return isObject
}

func hasKey(v any, key string) bool {
	_, ok := at(v, key)
	return ok
}
