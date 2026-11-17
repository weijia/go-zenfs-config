package zenfsconfig

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestPushCopiesFilesAndVersions(t *testing.T) {
	ctx := context.Background()
	src, dst := newMem(), newMem()
	r, _ := Open(ctx, src, "openlist")
	if _, err := r.SetConfig(ctx, "settings", map[string]any{"title": "a"}); err != nil {
		t.Fatal(err)
	}
	st, err := Push(ctx, src, dst, MirrorOptions{Prefixes: []string{"/openlist"}, Author: "go-test"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Written == 0 {
		t.Fatalf("expected writes, got %+v", st)
	}
	data, err := dst.ReadFile(ctx, "/openlist/settings.json")
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	want, _ := MarshalConfigValue(map[string]any{"title": "a"})
	if !bytes.Equal(data, want) {
		t.Fatalf("content mismatch")
	}
	if _, ok := dst.files["/openlist/settings.json.version"]; !ok {
		t.Fatalf("version sidecar not copied")
	}

	// Second push: everything skipped.
	st, err = Push(ctx, src, dst, MirrorOptions{Prefixes: []string{"/openlist"}})
	if err != nil || st.Written != 0 {
		t.Fatalf("idempotent push: %+v %v", st, err)
	}
}

func TestPushDeletesTargetOnlyFiles(t *testing.T) {
	ctx := context.Background()
	src, dst := newMem(), newMem()
	_ = dst.WriteFile(ctx, "/openlist/old.json", []byte("{}"))
	_, err := Push(ctx, src, dst, MirrorOptions{Prefixes: []string{"/openlist"}, Author: "go-test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := dst.files["/openlist/old.json"]; ok {
		t.Fatalf("file should be deleted")
	}
	ts, err := TombstoneAt(ctx, dst, "/openlist/old.json")
	if err != nil || ts == nil {
		t.Fatalf("tombstone missing: %v %v", ts, err)
	}
}

func TestPushKeepsRecreatedFile(t *testing.T) {
	ctx := context.Background()
	src, dst := newMem(), newMem()
	deletedAt := time.Now().UnixMilli() - 10_000
	body, _ := Tombstone{Path: "/openlist/keep.json", DeletedAt: deletedAt, DeletedBy: "x", ConfirmedBy: []string{}}.MarshalCanonical()
	_ = src.WriteFile(ctx, TombstonePath("/openlist/keep.json"), body)
	// Target re-created the file well after the deletion.
	_ = dst.WriteFile(ctx, "/openlist/keep.json", []byte("{}"))
	dst.mtime["/openlist/keep.json"] = time.Now()
	if _, err := Push(ctx, src, dst, MirrorOptions{Prefixes: []string{"/openlist"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := dst.files["/openlist/keep.json"]; !ok {
		t.Fatalf("recreated file must survive")
	}
}

func TestExcludedMetaRuntime(t *testing.T) {
	for _, p := range []string{"/.meta/tombstones/a.json", "/.meta/.deleted/b.json", "/.meta/versions/c.json", "/openlist/a.json.mtime", "/.keep"} {
		if !excluded(p) {
			t.Errorf("%s should be excluded", p)
		}
	}
	for _, p := range []string{"/openlist/a.json", "/openlist/a.json.version", "/.meta/group.json"} {
		if excluded(p) {
			t.Errorf("%s should be included", p)
		}
	}
}
