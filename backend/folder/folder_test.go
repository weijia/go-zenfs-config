package folder

import (
	"context"
	"testing"

	zc "github.com/weijia/go-zenfs-config"
)

func TestBackendRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	b, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.WriteFile(ctx, "/.meta/group.json", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	data, err := b.ReadFile(ctx, "/.meta/group.json")
	if err != nil || string(data) != "{}" {
		t.Fatalf("read: %q %v", data, err)
	}
	entries, err := b.List(ctx, "/.meta")
	if err != nil || len(entries) != 1 {
		t.Fatalf("list: %+v %v", entries, err)
	}
	if _, err := b.Stat(ctx, "/.meta/group.json"); err != nil {
		t.Fatalf("stat: %v", err)
	}
	if err := b.Remove(ctx, "/.meta/group.json"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := b.ReadFile(ctx, "/.meta/group.json"); err == nil {
		t.Fatalf("expected ErrNotExist")
	} else if !zc.IsNotExist(err) {
		t.Fatalf("expected ErrNotExist, got %v", err)
	}
}

func TestWalk(t *testing.T) {
	ctx := context.Background()
	b, _ := New(t.TempDir())
	_ = b.WriteFile(ctx, "/a/b.json", []byte("1"))
	_ = b.WriteFile(ctx, "/c.txt", []byte("2"))
	files, err := zc.Walk(ctx, b, "/")
	if err != nil || len(files) != 2 {
		t.Fatalf("walk: %+v %v", files, err)
	}
}
