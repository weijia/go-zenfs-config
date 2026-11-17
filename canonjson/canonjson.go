// Package canonjson reproduces the exact JSON byte layout used by
// zen-fs-config's canonical serializer:
//
//   - caller-controlled object key order (no implicit sorting),
//   - 2-space indentation, "\n" newlines,
//   - no trailing newline,
//   - non-ASCII characters kept as UTF-8 (no \u escaping),
//   - no HTML escaping ("<", ">", "&" stay literal),
//   - JS-compatible number formatting for integers and floats.
//
// The package deliberately does not expose a generic map encoder: callers
// must pass an Object (ordered pairs) so that the wire layout stays
// deterministic across devices and languages.
package canonjson

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Pair is one object member in the order it will be written.
type Pair struct {
	K string
	V any
}

// Object is an ordered JSON object. A nil Object encodes as {}.
type Object []Pair

// Array is a JSON array. A nil Array encodes as [].
type Array []any

// Marshal encodes v into canonical bytes.
//
// Supported values: Object, Array, string, bool, nil, all Go integer types,
// float32/float64, and json.Number. Anything else is an error so that
// accidental map usage cannot produce silently different bytes.
func Marshal(v any) ([]byte, error) {
	var b strings.Builder
	if err := writeValue(&b, v, 0); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

func writeValue(b *strings.Builder, v any, depth int) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case Object:
		return writeObject(b, x, depth)
	case Array:
		return writeArray(b, x, depth)
	case string:
		writeString(b, x)
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case int:
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case int8:
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case int16:
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case int32:
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case uint:
		b.WriteString(strconv.FormatUint(uint64(x), 10))
	case uint8:
		b.WriteString(strconv.FormatUint(uint64(x), 10))
	case uint16:
		b.WriteString(strconv.FormatUint(uint64(x), 10))
	case uint32:
		b.WriteString(strconv.FormatUint(uint64(x), 10))
	case uint64:
		b.WriteString(strconv.FormatUint(x, 10))
	case float32:
		return writeFloat(b, float64(x), 32)
	case float64:
		return writeFloat(b, x, 64)
	default:
		return fmt.Errorf("canonjson: unsupported type %T", v)
	}
	return nil
}

func writeObject(b *strings.Builder, o Object, depth int) error {
	if len(o) == 0 {
		b.WriteString("{}")
		return nil
	}
	b.WriteString("{\n")
	inner := strings.Repeat("  ", depth+1)
	for i, p := range o {
		b.WriteString(inner)
		writeString(b, p.K)
		b.WriteString(": ")
		if err := writeValue(b, p.V, depth+1); err != nil {
			return fmt.Errorf("canonjson: key %q: %w", p.K, err)
		}
		if i != len(o)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString(strings.Repeat("  ", depth))
	b.WriteString("}")
	return nil
}

func writeArray(b *strings.Builder, a Array, depth int) error {
	if len(a) == 0 {
		b.WriteString("[]")
		return nil
	}
	b.WriteString("[\n")
	inner := strings.Repeat("  ", depth+1)
	for i, v := range a {
		b.WriteString(inner)
		if err := writeValue(b, v, depth+1); err != nil {
			return fmt.Errorf("canonjson: index %d: %w", i, err)
		}
		if i != len(a)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString(strings.Repeat("  ", depth))
	b.WriteString("]")
	return nil
}

const hexDigits = "0123456789abcdef"

// writeString implements JSON.stringify's string quoting: only ", \ and
// control characters are escaped; everything else, including U+2028/U+2029
// and non-ASCII, is emitted as UTF-8.
func writeString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch c {
			case '"':
				b.WriteString(`\"`)
			case '\\':
				b.WriteString(`\\`)
			case '\b':
				b.WriteString(`\b`)
			case '\f':
				b.WriteString(`\f`)
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			default:
				if c < 0x20 {
					b.WriteString(`\u00`)
					b.WriteByte(hexDigits[c>>4])
					b.WriteByte(hexDigits[c&0xF])
				} else {
					b.WriteByte(c)
				}
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			// Invalid UTF-8 cannot appear in a JS string; use the replacement
			// character instead of emitting broken bytes.
			b.WriteString("\uFFFD")
			i++
			continue
		}
		b.WriteString(s[i : i+size])
		i += size
	}
	b.WriteByte('"')
}

// writeFloat renders a float the way JS Number→String does for the values
// that can appear in descriptors (integral timestamps and small decimals).
func writeFloat(b *strings.Builder, f float64, bits int) error {
	if f != f || f > 1.7976931348623157e308 || f < -1.7976931348623157e308 {
		return fmt.Errorf("canonjson: unsupported number %v", f)
	}
	if f == 0 {
		if 1/f < 0 {
			b.WriteString("0") // JSON.stringify(-0) is "0"
			return nil
		}
		b.WriteString("0")
		return nil
	}
	if f == float64(int64(f)) && f < 1e21 && f > -1e21 {
		b.WriteString(strconv.FormatInt(int64(f), 10))
		return nil
	}
	s := strconv.FormatFloat(f, 'g', -1, bits)
	// Go writes 1e-07 while JS writes 1e-7; normalize the exponent.
	if i := strings.IndexByte(s, 'e'); i >= 0 {
		mant, exp := s[:i], s[i+1:]
		sign := ""
		if strings.HasPrefix(exp, "+") || strings.HasPrefix(exp, "-") {
			sign = exp[:1]
			exp = exp[1:]
		}
		exp = strings.TrimLeft(exp, "0")
		if exp == "" {
			exp = "0"
		}
		if sign == "" {
			sign = "+"
		}
		s = mant + "e" + sign + exp
	}
	b.WriteString(s)
	return nil
}
