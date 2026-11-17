package zenfsconfig

import (
	"bytes"
	"context"
	"path"
	"testing"
	"time"
)

type memStorage struct {
	files map[string][]byte
	dirs  map[string]bool
	mtime map[string]time.Time
}

func newMem() *memStorage {
	return &memStorage{files: map[string][]byte{}, dirs: map[string]bool{"/": true}, mtime: map[string]time.Time{}}
}

func (m *memStorage) List(_ context.Context, dir string) ([]Entry, error) {
	if !m.dirs[dir] {
		return nil, ErrNotExist
	}
	var out []Entry
	for p, data := range m.files {
		if path.Dir(p) == dir {
			out = append(out, Entry{Path: p, Size: int64(len(data)), MTime: m.mtime[p]})
		}
	}
	for d := range m.dirs {
		if d != "/" && path.Dir(d) == dir {
			out = append(out, Entry{Path: d, Dir: true})
		}
	}
	return out, nil
}

func (m *memStorage) ReadFile(_ context.Context, p string) ([]byte, error) {
	data, ok := m.files[p]
	if !ok {
		return nil, ErrNotExist
	}
	return data, nil
}

func (m *memStorage) WriteFile(_ context.Context, p string, data []byte) error {
	m.files[p] = append([]byte(nil), data...)
	m.mtime[p] = time.Now()
	m.dirs[path.Dir(p)] = true
	return nil
}

func (m *memStorage) Remove(_ context.Context, p string) error {
	if _, ok := m.files[p]; !ok {
		return ErrNotExist
	}
	delete(m.files, p)
	delete(m.mtime, p)
	return nil
}

func (m *memStorage) MkdirAll(_ context.Context, dir string) error {
	m.dirs[dir] = true
	return nil
}

func (m *memStorage) Stat(_ context.Context, p string) (Entry, error) {
	if data, ok := m.files[p]; ok {
		return Entry{Path: p, Size: int64(len(data)), MTime: m.mtime[p]}, nil
	}
	if m.dirs[p] {
		return Entry{Path: p, Dir: true}, nil
	}
	return Entry{}, ErrNotExist
}

func TestEnsureGroupWritesRealBytes(t *testing.T) {
	ctx := context.Background()
	s := newMem()
	created, err := EnsureGroup(ctx, s, NewConfigGroup("config", ""))
	if err != nil || !created {
		t.Fatalf("ensure: created=%v err=%v", created, err)
	}
	want := fixture(t, "configs/group.json")
	if !bytes.Equal(s.files[GroupDescriptorPath], want) {
		t.Fatalf("group bytes differ\n got: %q\nwant: %q", s.files[GroupDescriptorPath], want)
	}
	for _, p := range []string{MetaGroupTypePath, CfgGroupTypePath} {
		if string(s.files[p]) != GroupTypeConfig {
			t.Fatalf("%s = %q", p, s.files[p])
		}
	}
	// A second call must not rewrite anything.
	again, err := EnsureGroup(ctx, s, NewConfigGroup("config", ""))
	if err != nil || again {
		t.Fatalf("second ensure: created=%v err=%v", again, err)
	}
}

func TestRepoSetReadDelete(t *testing.T) {
	ctx := context.Background()
	s := newMem()
	if _, err := EnsureGroup(ctx, s, NewConfigGroup("config", "")); err != nil {
		t.Fatal(err)
	}
	r, err := Open(ctx, s, "kids-point")
	if err != nil {
		t.Fatal(err)
	}
	r.Author = "go-test"
	r.Now = func() time.Time { return time.UnixMilli(1794153088403) }

	value := map[string]any{"b": 2, "a": 1}
	changed, err := r.SetConfig(ctx, "settings", value)
	if err != nil || !changed {
		t.Fatalf("set: %v %v", changed, err)
	}
	want, _ := MarshalConfigValue(value)
	if !bytes.Equal(s.files["/kids-point/settings.json"], want) {
		t.Fatalf("content bytes differ")
	}
	vm, err := r.ReadVersion(ctx, "/kids-point/settings.json")
	if err != nil || vm == nil || vm.Version != 1 {
		t.Fatalf("version: %+v %v", vm, err)
	}

	// Same semantic value, different byte order: no write.
	changed, err = r.SetConfig(ctx, "settings", value)
	if err != nil || changed {
		t.Fatalf("idempotent: %v %v", changed, err)
	}

	changed, err = r.SetConfig(ctx, "settings", map[string]any{"a": 9})
	if err != nil || !changed {
		t.Fatalf("second set: %v %v", changed, err)
	}
	vm, _ = r.ReadVersion(ctx, "/kids-point/settings.json")
	if vm.Version != 2 {
		t.Fatalf("version = %d, want 2", vm.Version)
	}

	if err := r.DeleteConfig(ctx, "settings"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadConfig(ctx, "settings"); err == nil {
		t.Fatalf("file should be deleted")
	}
	ts, err := TombstoneAt(ctx, s, "/kids-point/settings.json")
	if err != nil || ts == nil {
		t.Fatalf("tombstone: %+v %v", ts, err)
	}
	if ts.DeletedBy != "go-test" {
		t.Fatalf("deletedBy = %q", ts.DeletedBy)
	}
}

func TestNodeConfigPathHasNoAppPrefix(t *testing.T) {
	r := &Repo{AppID: "kids-point"}
	if got := r.NodeConfigPath("n1", "env"); got != "/nodes/n1/env.json" {
		t.Fatalf("node path = %q", got)
	}
	if got := r.ConfigPath("db/host"); got != "/kids-point/db/host.json" {
		t.Fatalf("config path = %q", got)
	}
}

func TestProbeGroupKindPrefersGroupType(t *testing.T) {
	ctx := context.Background()
	s := newMem()
	_ = s.WriteFile(ctx, CfgGroupTypePath, []byte("data-sync"))
	kind, err := ProbeGroupKind(ctx, s)
	if err != nil || kind != KindData {
		t.Fatalf("kind=%q err=%v", kind, err)
	}
	// group.json only: data group (pim-store shape).
	s2 := newMem()
	_ = s2.WriteFile(ctx, GroupDescriptorPath, fixture(t, "pim/group.json"))
	kind, err = ProbeGroupKind(ctx, s2)
	if err != nil || kind != KindData {
		t.Fatalf("group.json probe: kind=%q err=%v", kind, err)
	}
}
