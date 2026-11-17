package zenfsconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

func fixedClock(ms int64) func() time.Time {
	return func() time.Time { return time.UnixMilli(ms) }
}

func TestDetectGroupType(t *testing.T) {
	ctx := context.Background()

	t.Run("config marker", func(t *testing.T) {
		s := newMem()
		s.files[CfgGroupTypePath] = []byte(GroupTypeConfig + "\n")
		got, found, err := DetectGroupType(ctx, s)
		if err != nil || !found || got != GroupTypeConfig {
			t.Fatalf("got=%q found=%v err=%v", got, found, err)
		}
	})

	t.Run("legacy meta marker", func(t *testing.T) {
		s := newMem()
		s.files[MetaGroupTypePath] = []byte(GroupTypeData)
		got, found, err := DetectGroupType(ctx, s)
		if err != nil || !found || got != GroupTypeData {
			t.Fatalf("got=%q found=%v err=%v", got, found, err)
		}
	})

	t.Run("unknown marker is an error, not config", func(t *testing.T) {
		s := newMem()
		s.files[CfgGroupTypePath] = []byte("config-sync-typo")
		if _, _, err := DetectGroupType(ctx, s); !errors.Is(err, ErrUnknownGroupType) {
			t.Fatalf("want ErrUnknownGroupType, got %v", err)
		}
	})

	t.Run("group.json fallback", func(t *testing.T) {
		s := newMem()
		g := NewDataGroup("pim", "PIM")
		body, err := g.MarshalCanonical()
		if err != nil {
			t.Fatal(err)
		}
		s.files[GroupDescriptorPath] = body
		got, found, err := DetectGroupType(ctx, s)
		if err != nil || !found || got != GroupTypeData {
			t.Fatalf("got=%q found=%v err=%v", got, found, err)
		}
	})

	t.Run("empty remote", func(t *testing.T) {
		s := newMem()
		got, found, err := DetectGroupType(ctx, s)
		if err != nil || found || got != "" {
			t.Fatalf("got=%q found=%v err=%v", got, found, err)
		}
		if rt, err := ResolveGroupType(ctx, s, ""); err != nil || rt != GroupTypeConfig {
			t.Fatalf("resolve default: %q %v", rt, err)
		}
	})
}

func TestAppDataGroupDescriptorBytes(t *testing.T) {
	d := AppDataGroupDescriptor{
		ID:        "default",
		GroupType: GroupTypeData,
		Backends: []BackendDescriptor{
			{ID: "gitee-primary", Type: "gitee", Options: map[string]any{"repo": "pim-store", "owner": "weijia432"}},
		},
	}
	body, err := d.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n" +
		"  \"id\": \"default\",\n" +
		"  \"groupType\": \"data-sync\",\n" +
		"  \"backends\": [\n" +
		"    {\n" +
		"      \"id\": \"gitee-primary\",\n" +
		"      \"type\": \"gitee\",\n" +
		"      \"options\": {\n" +
		"        \"owner\": \"weijia432\",\n" +
		"        \"repo\": \"pim-store\"\n" +
		"      }\n" +
		"    }\n" +
		"  ]\n" +
		"}"
	if string(body) != want {
		t.Fatalf("canonical bytes differ\n got: %q\nwant: %q", body, want)
	}
	// Optional fields stay absent rather than null.
	if bytes.Contains(body, []byte("null")) || bytes.Contains(body, []byte("name")) {
		t.Fatalf("optional fields leaked: %q", body)
	}
}

func TestAppDataGroupDescriptorRealFileRoundTrip(t *testing.T) {
	raw := fixture(t, "configs/app-data-group.json")
	var d AppDataGroupDescriptor
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	if d.ID != "default" || d.GroupType != GroupTypeData || len(d.Backends) != 2 {
		t.Fatalf("decoded descriptor = %+v", d)
	}
	body, err := d.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, raw) {
		t.Fatalf("round-trip differs\n got: %s\nwant: %s", body, raw)
	}
	proj, err := d.MarshalProjection()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(proj, []byte("createdAt")) {
		t.Fatalf("projection form must drop createdAt:\n%s", proj)
	}
}

func TestSaveAppDataGroupDescriptorIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newMem()
	r := &Repo{Storage: s, AppID: "openlist", Author: "openlist/node-1", Now: fixedClock(1000)}

	d := AppDataGroupDescriptor{
		ID:        "default",
		GroupType: GroupTypeData,
		Backends: []BackendDescriptor{
			{ID: "gitee-primary", Type: "gitee", Options: map[string]any{"owner": "weijia432", "repo": "pim-store"}, CreatedAt: ptr(int64(500))},
		},
	}
	changed, err := r.SaveAppDataGroupDescriptor(ctx, d)
	if err != nil || !changed {
		t.Fatalf("first save: changed=%v err=%v", changed, err)
	}
	p := AppDataGroupPath("openlist", "default")
	if _, ok := s.files[p]; !ok {
		t.Fatalf("descriptor not written at %s", p)
	}
	// A later createdAt must not force a rewrite.
	d2 := d
	d2.Backends = []BackendDescriptor{d.Backends[0]}
	d2.Backends[0].CreatedAt = ptr(int64(9999))
	changed, err = r.SaveAppDataGroupDescriptor(ctx, d2)
	if err != nil || changed {
		t.Fatalf("projection-equal save must be a no-op: changed=%v err=%v", changed, err)
	}
	// Merging keeps the earliest createdAt on disk.
	var stored AppDataGroupDescriptor
	if err := json.Unmarshal(s.files[p], &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Backends[0].CreatedAt == nil || *stored.Backends[0].CreatedAt != 500 {
		t.Fatalf("createdAt = %v, want 500", stored.Backends[0].CreatedAt)
	}
}

func TestCreateAppDataGroupAndDataFiles(t *testing.T) {
	ctx := context.Background()
	configStore := newMem()
	dataStore := newMem()
	r := &Repo{Storage: configStore, AppID: "openlist", Author: "openlist/node-1", Now: fixedClock(2000)}

	dg, err := r.CreateAppDataGroup(ctx, "default", []BackendDescriptor{
		{ID: "gitee-primary", Type: "gitee", Options: map[string]any{"owner": "weijia432", "repo": "pim-store"}},
	}, CreateAppDataGroupOptions{Storage: dataStore, Name: ptr("PIM")})
	if err != nil {
		t.Fatal(err)
	}

	// Data group bootstraps its own descriptor with data features.
	var g GroupDescriptor
	if err := json.Unmarshal(dataStore.files[GroupDescriptorPath], &g); err != nil {
		t.Fatal(err)
	}
	if g.Kind != KindData || g.ID != "default" || g.Name != "PIM" {
		t.Fatalf("group descriptor = %+v", g)
	}
	if g.Features != DataFeatures() {
		t.Fatalf("features = %+v", g.Features)
	}
	if string(dataStore.files[CfgGroupTypePath]) != GroupTypeData || string(dataStore.files[MetaGroupTypePath]) != GroupTypeData {
		t.Fatalf("group-type markers not written")
	}

	// The authoritative backend descriptor lives in the data group.
	if _, ok := dataStore.files["/.meta/backends/gitee-primary.json"]; !ok {
		t.Fatalf("backend descriptor missing")
	}
	// And the bootstrap copy in the config group mirrors it.
	groups, err := r.ListAppDataGroups(ctx)
	if err != nil || len(groups) != 1 {
		t.Fatalf("list: %v err=%v", groups, err)
	}
	if groups[0].ID != "default" || groups[0].GroupType != GroupTypeData || len(groups[0].Backends) != 1 {
		t.Fatalf("descriptor = %+v", groups[0])
	}
	if groups[0].Name == nil || *groups[0].Name != "PIM" {
		t.Fatalf("name = %v", groups[0].Name)
	}

	// User files: write, idempotent rewrite, version record, read.
	changed, err := dg.WriteFile(ctx, "/notes/todo.json", []byte("{\"a\":1}"))
	if err != nil || !changed {
		t.Fatalf("write: changed=%v err=%v", changed, err)
	}
	again, err := dg.WriteFile(ctx, "/notes/todo.json", []byte("{\"a\":1}"))
	if err != nil || again {
		t.Fatalf("rewrite must be a no-op: %v %v", again, err)
	}
	if _, ok := dataStore.files["/notes/todo.json"]; !ok {
		t.Fatalf("user file missing")
	}
	// Versions are off by default for data groups.
	if _, ok := dataStore.files[DataVersionPath("/notes/todo.json")]; ok {
		t.Fatalf("version record written although features.versions=false")
	}

	// Reserved namespaces are rejected.
	for _, p := range []string{"/.meta/x", "/.config/x", "/.meta"} {
		if _, err := dg.UserPath(p); err == nil {
			t.Fatalf("%s must be rejected", p)
		}
	}

	// Delete writes a tombstone and removes the file.
	if err := dg.DeleteFile(ctx, "/notes/todo.json"); err != nil {
		t.Fatal(err)
	}
	if _, ok := dataStore.files["/notes/todo.json"]; ok {
		t.Fatalf("file not removed")
	}
	tb, ok := dataStore.files[TombstonePath("/notes/todo.json")]
	if !ok {
		t.Fatalf("tombstone missing")
	}
	if !bytes.Contains(tb, []byte(`"deletedBy": "openlist/node-1"`)) {
		t.Fatalf("tombstone = %s", tb)
	}

	// Removing the registration leaves the data group's own files alone.
	if err := r.RemoveAppDataGroup(ctx, "default"); err != nil {
		t.Fatal(err)
	}
	if _, ok := configStore.files[AppDataGroupPath("openlist", "default")]; ok {
		t.Fatalf("bootstrap copy not removed")
	}
	if _, ok := dataStore.files["/.meta/backends/gitee-primary.json"]; !ok {
		t.Fatalf("data group backend must survive removal of the registration")
	}
}

func TestSharedRemoteRejected(t *testing.T) {
	ctx := context.Background()
	configStore := newMem()
	r := &Repo{Storage: configStore, AppID: "openlist", Author: "test", Now: fixedClock(3000)}

	// Config group already points at the same remote.
	configBackend := BackendDescriptor{ID: "gitee-config", Type: "gitee", Options: map[string]any{"owner": "weijia432", "repo": "pim-store"}}
	body, err := configBackend.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	if err := configStore.WriteFile(ctx, "/.meta/backends/gitee-config.json", body); err != nil {
		t.Fatal(err)
	}

	_, err = r.CreateAppDataGroup(ctx, "default", []BackendDescriptor{
		{ID: "gitee-primary", Type: "gitee", Options: map[string]any{"repo": "pim-store", "owner": "weijia432"}},
	}, CreateAppDataGroupOptions{Storage: newMem()})
	if err == nil || !strings.Contains(err.Error(), "same remote") {
		t.Fatalf("want shared-remote rejection, got %v", err)
	}

	// A different repo is fine.
	_, err = r.CreateAppDataGroup(ctx, "other", []BackendDescriptor{
		{ID: "gitee-primary", Type: "gitee", Options: map[string]any{"owner": "weijia432", "repo": "configs"}},
	}, CreateAppDataGroupOptions{Storage: newMem()})
	if err != nil {
		t.Fatalf("distinct remote must be accepted: %v", err)
	}
}

func TestConnectConfigRoute(t *testing.T) {
	ctx := context.Background()
	s := newMem()
	s.files[CfgGroupTypePath] = []byte(GroupTypeConfig)

	res, err := Connect(ctx, ConnectOptions{AppID: "openlist", Storage: s, Author: "openlist/node-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsConfig() || res.Data != nil || res.Repo == nil {
		t.Fatalf("res = %+v", res)
	}
	if _, err := Connect(ctx, ConnectOptions{AppID: "openlist", Storage: s, ExpectType: GroupTypeData}); !errors.Is(err, ErrGroupTypeMismatch) {
		t.Fatalf("want mismatch, got %v", err)
	}
}

func TestConnectDataRoute(t *testing.T) {
	ctx := context.Background()
	configStore := newMem()
	dataStore := newMem()
	dataStore.files[CfgGroupTypePath] = []byte(GroupTypeData)

	res, err := Connect(ctx, ConnectOptions{
		AppID:         "openlist",
		Storage:       dataStore,
		ConfigStorage: configStore,
		DataGroupName: "PIM",
		Backend: &BackendDescriptor{
			ID: "gitee-primary", Type: "gitee",
			Options: map[string]any{"owner": "weijia432", "repo": "pim-store"},
		},
		Author: "openlist/node-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsData() || res.Data == nil || res.Repo == nil {
		t.Fatalf("res = %+v", res)
	}
	if res.Data.ID != "default" {
		t.Fatalf("default data group id = %q", res.Data.ID)
	}
	// The config group is the host: it carries the bootstrap copy.
	if _, ok := configStore.files[AppDataGroupPath("openlist", "default")]; !ok {
		t.Fatalf("bootstrap descriptor not registered in the config group")
	}
	// The data group's own descriptor stays authoritative.
	if _, ok := dataStore.files["/.meta/backends/gitee-primary.json"]; !ok {
		t.Fatalf("authoritative backend descriptor missing")
	}

	// Connecting twice must not duplicate or rewrite anything.
	before := len(configStore.files)
	res2, err := Connect(ctx, ConnectOptions{
		AppID: "openlist", Storage: dataStore, ConfigStorage: configStore,
		Backend: &BackendDescriptor{ID: "gitee-primary", Type: "gitee", Options: map[string]any{"owner": "weijia432", "repo": "pim-store"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res2.IsData() || len(configStore.files) != before {
		t.Fatalf("second connect changed files: %d -> %d", before, len(configStore.files))
	}
}

func TestDataGroupVersionsWhenEnabled(t *testing.T) {
	ctx := context.Background()
	s := newMem()
	features := DataFeatures()
	features.Versions = true
	g := NewDataGroup("pim", "PIM")
	g.Features = features
	if _, err := EnsureGroup(ctx, s, g); err != nil {
		t.Fatal(err)
	}
	dg, err := OpenDataGroup(ctx, s, "openlist", "pim")
	if err != nil {
		t.Fatal(err)
	}
	dg.Features = features
	dg.Author = "openlist/node-1"
	dg.Now = fixedClock(4000)

	if _, err := dg.WriteFile(ctx, "/a/b.txt", []byte("one")); err != nil {
		t.Fatal(err)
	}
	vp := DataVersionPath("/a/b.txt")
	var meta VersionMeta
	if err := json.Unmarshal(s.files[vp], &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Version != 1 || meta.Hash != HashBytes([]byte("one")) || meta.Author != "openlist/node-1" {
		t.Fatalf("version = %+v", meta)
	}
	if _, err := dg.WriteFile(ctx, "/a/b.txt", []byte("two")); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(s.files[vp], &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Version != 2 {
		t.Fatalf("version did not bump: %+v", meta)
	}
	// Metadata files never get version records.
	if DataVersionPath("/.meta/group.json") != "" {
		t.Fatalf("metadata paths must not have version records")
	}
}

func TestDataSyncExcluded(t *testing.T) {
	cases := map[string]bool{
		"/.config/x":  true,
		"/nodes/n1":   true,
		"/shared/x":   true,
		"/.meta/x":    false,
		"/notes/a.md": false,
		"/configs":    false,
	}
	for p, want := range cases {
		if got := DataSyncExcluded(p); got != want {
			t.Fatalf("%s = %v, want %v", p, got, want)
		}
	}
}

func TestOpenDataGroupRejectsConfigGroup(t *testing.T) {
	ctx := context.Background()
	s := newMem()
	if _, err := EnsureGroup(ctx, s, NewConfigGroup("config", "")); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDataGroup(ctx, s, "openlist", "default"); !errors.Is(err, ErrGroupTypeMismatch) {
		t.Fatalf("want ErrGroupTypeMismatch, got %v", err)
	}
}
