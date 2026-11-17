package canonjson

import (
	"bytes"
	"testing"
)

func TestLayoutMatchesJSONStringify(t *testing.T) {
	out, err := Marshal(Object{
		{K: "a", V: 1},
		{K: "b", V: Array{1, "x"}},
		{K: "c", V: Object{}},
		{K: "d", V: Array{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"a\": 1,\n  \"b\": [\n    1,\n    \"x\"\n  ],\n  \"c\": {},\n  \"d\": []\n}"
	if string(out) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
	if out[len(out)-1] == '\n' {
		t.Fatalf("must not end with newline")
	}
}

func TestStringEscaping(t *testing.T) {
	out, err := Marshal(Object{
		{K: "s", V: "a\"b\\c\nd\te\x01f<&>\u2028"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"s\": \"a\\\"b\\\\c\\nd\\te\\u0001f<&>\u2028\"\n}"
	if string(out) != want {
		t.Fatalf("got: %q\nwant: %q", out, want)
	}
}

func TestNumbers(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{int64(1793522166597), "1793522166597"},
		{int(0), "0"},
		{float64(1.5), "1.5"},
		{float64(1e-7), "1e-7"},
		{float64(1e21), "1e+21"},
	}
	for _, tc := range cases {
		out, err := Marshal(tc.in)
		if err != nil {
			t.Fatalf("%v: %v", tc.in, err)
		}
		if string(out) != tc.want {
			t.Errorf("%v: got %q, want %q", tc.in, out, tc.want)
		}
	}
}

func TestLessUTF16DiffersFromByteOrder(t *testing.T) {
	// U+FFFF sorts after the emoji surrogate pair in UTF-16, but its UTF-8
	// first byte (0xEF) sorts before the emoji's (0xF0).
	a, b := "😀", "\uFFFF"
	if !LessUTF16(a, b) {
		t.Fatalf("LessUTF16(%q, %q) = false, want true", a, b)
	}
	if bytes.Compare([]byte(a), []byte(b)) < 0 {
		t.Fatalf("test assumption broken: UTF-8 order already agrees")
	}
}

func TestSortedObject(t *testing.T) {
	o := SortedObject(map[string]any{"b": 2, "a": 1, "😀": 3, "\uFFFF": 4})
	if len(o) != 4 || o[0].K != "a" || o[1].K != "b" {
		t.Fatalf("unexpected order: %+v", o)
	}
}
