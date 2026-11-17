package zenfsconfig

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func TestGroupDescriptorGolden(t *testing.T) {
	cases := []struct {
		file string
		size int
	}{
		{"configs/group.json", 197},
		{"pim/group.json", 211},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			raw := fixture(t, tc.file)
			if len(raw) != tc.size {
				t.Fatalf("fixture size = %d, want %d", len(raw), tc.size)
			}
			var g struct {
				SchemaVersion int      `json:"schemaVersion"`
				Kind          string   `json:"kind"`
				ID            string   `json:"id"`
				Name          string   `json:"name"`
				Features      Features `json:"features"`
			}
			if err := json.Unmarshal(raw, &g); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			out, err := GroupDescriptor{
				SchemaVersion: g.SchemaVersion,
				Kind:          g.Kind,
				ID:            g.ID,
				Name:          g.Name,
				Features:      g.Features,
			}.MarshalCanonical()
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !bytes.Equal(out, raw) {
				t.Fatalf("bytes differ\n got: %q\nwant: %q", out, raw)
			}
		})
	}
}

func TestBackendDescriptorGolden(t *testing.T) {
	cases := []struct {
		file string
		size int
	}{
		{"configs/backend-gitee.json", 198},
		{"pim/backend.json", 241},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			raw := fixture(t, tc.file)
			if len(raw) != tc.size {
				t.Fatalf("fixture size = %d, want %d", len(raw), tc.size)
			}
			var b struct {
				ID               string         `json:"id"`
				Type             string         `json:"type"`
				Options          map[string]any `json:"options"`
				AccountBackendID *string        `json:"accountBackendId"`
				Description      *string        `json:"description"`
				CreatedAt        *int64         `json:"createdAt"`
			}
			if err := json.Unmarshal(raw, &b); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			out, err := BackendDescriptor{
				ID:               b.ID,
				Type:             b.Type,
				Options:          b.Options,
				AccountBackendID: b.AccountBackendID,
				Description:      b.Description,
				CreatedAt:        b.CreatedAt,
			}.MarshalCanonical()
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !bytes.Equal(out, raw) {
				t.Fatalf("bytes differ\n got: %q\nwant: %q", out, raw)
			}
		})
	}
}

func TestVersionMetaGolden(t *testing.T) {
	raw := fixture(t, "configs/settings.version")
	if len(raw) != 180 {
		t.Fatalf("fixture size = %d, want 180", len(raw))
	}
	var v VersionMeta
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := v.MarshalCanonical()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Equal(out, raw) {
		t.Fatalf("bytes differ\n got: %q\nwant: %q", out, raw)
	}
}

func TestTombstoneGolden(t *testing.T) {
	cases := []struct {
		file string
		name string
		size int
	}{
		{"configs/tombstone-legacy.json", "++config__backends__gitee++json.json", 142},
		{"pim/tombstone.json", "pim-e2e__contacts__++sync++lock.json", 142},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			raw := fixture(t, tc.file)
			if len(raw) != tc.size {
				t.Fatalf("fixture size = %d, want %d", len(raw), tc.size)
			}
			parsed, problems := ParseTombstoneFile(raw, tc.name)
			if len(problems) != 0 {
				t.Fatalf("unexpected problems: %v", problems)
			}
			if len(parsed) != 1 {
				t.Fatalf("parsed %d tombstones, want 1", len(parsed))
			}
			out, err := parsed[0].MarshalCanonical()
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !bytes.Equal(out, raw) {
				t.Fatalf("bytes differ\n got: %q\nwant: %q", out, raw)
			}
		})
	}
}

func TestParseTombstoneRecoversConcatenatedBodies(t *testing.T) {
	body := fixture(t, "pim/tombstone.json")
	// A writer died mid-body and another writer appended a complete record:
	// only the second object is recoverable, and it must still be honoured.
	combined := append([]byte(nil), body[:len(body)-6]...)
	combined = append(combined, body...)
	parsed, _ := ParseTombstoneFile(combined, "pim-e2e__contacts__++sync++lock.json")
	if len(parsed) != 1 {
		t.Fatalf("recovered %d tombstones, want 1", len(parsed))
	}
	if parsed[0].Path != "/pim-e2e/contacts/.sync.lock" {
		t.Fatalf("path = %q", parsed[0].Path)
	}
}

func TestParseTombstoneFallsBackToFileName(t *testing.T) {
	parsed, problems := ParseTombstoneFile([]byte("not json at all"), "a__b++json.json")
	if len(problems) == 0 {
		t.Fatalf("expected a problem report")
	}
	if len(parsed) != 1 {
		t.Fatalf("parsed %d, want 1", len(parsed))
	}
	if parsed[0].Path != "/a/b.json" || parsed[0].DeletedAt != 0 || parsed[0].DeletedBy != "unknown" {
		t.Fatalf("unexpected fallback: %+v", parsed[0])
	}
}

func TestEncodeDecodeMetaPath(t *testing.T) {
	cases := map[string]string{
		"/pim-e2e/contacts/.sync.lock": "pim-e2e__contacts__++sync++lock",
		"/a/b.json":                    "a__b++json",
		"/.config/backends/gitee.json": "++config__backends__gitee++json",
	}
	for in, want := range cases {
		if got := EncodeMetaPath(in); got != want {
			t.Errorf("EncodeMetaPath(%q) = %q, want %q", in, got, want)
		}
		if got, ok := DecodeMetaPathEntry(want + ".json"); !ok || got != in {
			t.Errorf("DecodeMetaPathEntry(%q) = %q,%v want %q", want+".json", got, ok, in)
		}
	}
	if got := EncodeMetaPath("a/b.json"); got != "a__b++json" {
		t.Errorf("relative path should be normalised, got %q", got)
	}
}

func TestMarshalConfigValueIsStableAndNewlineFree(t *testing.T) {
	raw := fixture(t, "configs/settings.json")
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := MarshalConfigValue(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(out) == 0 || out[len(out)-1] == '\n' {
		t.Fatalf("output must not end with a newline")
	}
	var again map[string]any
	if err := json.Unmarshal(out, &again); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	out2, err := MarshalConfigValue(again)
	if err != nil {
		t.Fatalf("marshal 2: %v", err)
	}
	if !bytes.Equal(out, out2) {
		t.Fatalf("serialisation is not stable")
	}
}
