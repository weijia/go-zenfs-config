package canonjson

import "unicode/utf16"

// LessUTF16 compares two strings the way JavaScript's `<` operator does:
// by UTF-16 code units, not by UTF-8 bytes. This matters for keys that
// contain astral characters (emoji), where UTF-8 byte order and UTF-16
// code-unit order disagree.
func LessUTF16(a, b string) bool {
	ua := utf16.Encode([]rune(a))
	ub := utf16.Encode([]rune(b))
	n := len(ua)
	if len(ub) < n {
		n = len(ub)
	}
	for i := 0; i < n; i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

// SortedObject builds an Object from m with keys ordered by LessUTF16.
func SortedObject(m map[string]any) Object {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	o := make(Object, 0, len(keys))
	for _, k := range keys {
		o = append(o, Pair{K: k, V: m[k]})
	}
	return o
}

func sortStrings(s []string) {
	// Avoid importing sort in the hot path? sort is fine; insertion sort
	// keeps this stable and cheap for the small option maps we handle.
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && LessUTF16(s[j], s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
