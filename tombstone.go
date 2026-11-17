package zenfsconfig

import (
	"encoding/json"
	"strings"

	"github.com/weijia/go-zenfs-config/canonjson"
)

// Tombstone is a deletion record stored under /.meta/tombstones/.
type Tombstone struct {
	Path        string
	DeletedAt   int64
	DeletedBy   string
	ConfirmedBy []string
}

// MarshalCanonical returns the exact bytes written by group-meta.ts.
func (t Tombstone) MarshalCanonical() ([]byte, error) {
	arr := make(canonjson.Array, 0, len(t.ConfirmedBy))
	for _, id := range t.ConfirmedBy {
		arr = append(arr, id)
	}
	return canonjson.Marshal(canonjson.Object{
		{K: "path", V: t.Path},
		{K: "deletedAt", V: t.DeletedAt},
		{K: "deletedBy", V: t.DeletedBy},
		{K: "confirmedBy", V: arr},
	})
}

// ParseTombstoneFile decodes a tombstone body the way group-meta.ts does:
//
//   - if the file was truncated or two writers concatenated their bodies,
//     every balanced {...} object is extracted and all valid records are kept;
//   - if nothing is recoverable but the file name encodes a path, a minimal
//     tombstone is synthesised with deletedAt=0, deletedBy="unknown";
//   - the result is always treated as "deleted" (conservative).
//
// entryName is the file name inside the tombstones directory.
func ParseTombstoneFile(raw []byte, entryName string) ([]Tombstone, []string) {
	fallback, _ := DecodeMetaPathEntry(entryName)
	var out []Tombstone
	var problems []string
	for _, obj := range extractJSONObjects(string(raw)) {
		var v map[string]any
		dec := json.NewDecoder(strings.NewReader(obj))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			problems = append(problems, "undecodable object")
			continue
		}
		if t, ok := normalizeTombstone(v, fallback); ok {
			out = append(out, t)
		} else {
			problems = append(problems, "object has no usable path")
		}
	}
	if len(out) == 0 && fallback != "" {
		out = append(out, Tombstone{Path: fallback, DeletedAt: 0, DeletedBy: "unknown", ConfirmedBy: []string{}})
		problems = append(problems, "unrecoverable body; fell back to file name")
	}
	return out, problems
}

func normalizeTombstone(v map[string]any, fallbackPath string) (Tombstone, bool) {
	rawPath, _ := v["path"].(string)
	if rawPath == "" {
		rawPath = fallbackPath
	}
	if rawPath == "" {
		return Tombstone{}, false
	}
	if !strings.HasPrefix(rawPath, "/") {
		rawPath = "/" + rawPath
	}
	t := Tombstone{Path: rawPath, DeletedBy: "unknown", ConfirmedBy: []string{}}
	switch n := v["deletedAt"].(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil {
			t.DeletedAt = i
		}
	case float64:
		t.DeletedAt = int64(n)
	}
	if s, ok := v["deletedBy"].(string); ok && s != "" {
		t.DeletedBy = s
	}
	if list, ok := v["confirmedBy"].([]any); ok {
		for _, item := range list {
			if s, ok := item.(string); ok {
				t.ConfirmedBy = append(t.ConfirmedBy, s)
			}
		}
	}
	return t, true
}

// extractJSONObjects finds every balanced {...} object in raw. Braces inside
// strings (including escaped quotes) are ignored, mirroring the JS scanner.
func extractJSONObjects(raw string) []string {
	var out []string
	depth := 0
	start := -1
	inString := false
	escaped := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					out = append(out, raw[start:i+1])
					start = -1
				}
			}
		}
	}
	return out
}
