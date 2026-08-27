package extract

import "reflect"

// isNilInterface reports whether e holds a nil pointer inside a non-nil
// interface — the classic Go trap that would let NewChain(myNilExtractor)
// build a chain that panics on first use rather than skipping the entry.
func isNilInterface(e Extractor) bool {
	v := reflect.ValueOf(e)
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return v.IsNil()
	default:
		return false
	}
}
