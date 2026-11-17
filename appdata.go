package zenfsconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/weijia/go-zenfs-config/canonjson"
)

// ErrInvalidID is returned for identifiers that would escape a metadata
// directory when used as a file name.
var ErrInvalidID = errors.New("zenfsconfig: invalid id")

// AppDataGroupPath returns the bootstrap descriptor path used by current
// clients: /.config/app-data-groups/{appId}/{id}.json.
func AppDataGroupPath(appID, id string) string {
	return AppDataGroupsDir + "/" + appID + "/" + id + ".json"
}

// LegacyAppDataGroupPath returns the pre-0.6 bootstrap descriptor path.
func LegacyAppDataGroupPath(appID, id string) string {
	return LegacyAppDataDir + "/" + appID + "/" + id + ".json"
}

// DataVersionPath returns the central version record path for a data-group
// file: /.meta/versions/{encoded}.json. Metadata paths and version sidecars
// have no version record (mirrors VersionStore.pathFor).
func DataVersionPath(filePath string) string {
	p := filePath
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if strings.HasPrefix(p, MetaDir+"/") || strings.HasSuffix(p, ".version") {
		return ""
	}
	return path.Join(VersionsDir, EncodeMetaPath(p)+".json")
}

// DataSyncExcluded reports whether p is outside a data group's synced area.
// Data groups never sync /.config, /nodes or /shared; .meta stays in scope.
func DataSyncExcluded(p string) bool {
	clean := path.Clean("/" + strings.TrimPrefix(p, "/"))
	for _, prefix := range []string{ConfigDir, "/nodes", "/shared"} {
		if clean == prefix || strings.HasPrefix(clean, prefix+"/") {
			return true
		}
	}
	return false
}

func assertValidID(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: id must be non-empty", ErrInvalidID)
	}
	if strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return fmt.Errorf("%w: %q must not contain %q, %q or %q", ErrInvalidID, id, "/", `\`, "..")
	}
	return nil
}

// ListAppDataGroups returns every registered data group. The current directory
// wins over the legacy mirror; malformed descriptors are skipped.
func (r *Repo) ListAppDataGroups(ctx context.Context) ([]AppDataGroupDescriptor, error) {
	seen := map[string]bool{}
	out := make([]AppDataGroupDescriptor, 0)
	for _, dir := range []string{AppDataGroupsDir + "/" + r.AppID, LegacyAppDataDir + "/" + r.AppID} {
		entries, err := r.Storage.List(ctx, dir)
		if err != nil {
			if errors.Is(err, ErrNotExist) {
				continue
			}
			return nil, err
		}
		for _, e := range entries {
			if e.Dir || !strings.HasSuffix(e.Path, ".json") || strings.HasSuffix(e.Path, ".version") {
				continue
			}
			data, err := r.Storage.ReadFile(ctx, e.Path)
			if err != nil {
				continue
			}
			var d AppDataGroupDescriptor
			if err := json.Unmarshal(data, &d); err != nil || d.ID == "" {
				continue
			}
			if _, err := ParseGroupType(d.GroupType); err != nil {
				continue
			}
			id := path.Base(e.Path)
			id = strings.TrimSuffix(id, ".json")
			if seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, normalizeAppDataDescriptor(d, nil))
		}
	}
	sort.Slice(out, func(i, j int) bool { return canonjson.LessUTF16(out[i].ID, out[j].ID) })
	return out, nil
}

// GetAppDataGroupDescriptor returns one registered descriptor, current path
// first, then the legacy mirror.
func (r *Repo) GetAppDataGroupDescriptor(ctx context.Context, id string) (*AppDataGroupDescriptor, error) {
	if err := assertValidID(id); err != nil {
		return nil, err
	}
	for _, p := range []string{AppDataGroupPath(r.AppID, id), LegacyAppDataGroupPath(r.AppID, id)} {
		data, ok, err := ReadFileOrNil(ctx, r.Storage, p)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		var d AppDataGroupDescriptor
		if err := json.Unmarshal(data, &d); err != nil {
			return nil, fmt.Errorf("zenfsconfig: decode app-data-group %q: %w", id, err)
		}
		if d.ID == "" {
			d.ID = id
		}
		if _, err := ParseGroupType(d.GroupType); err != nil {
			return nil, err
		}
		norm := normalizeAppDataDescriptor(d, nil)
		return &norm, nil
	}
	return nil, ErrNotExist
}

// SaveAppDataGroupDescriptor writes the bootstrap copy, merging createdAt with
// the file already on disk and skipping the write when the projection bytes are
// unchanged. The data group's own .meta/backends stays authoritative.
func (r *Repo) SaveAppDataGroupDescriptor(ctx context.Context, d AppDataGroupDescriptor) (bool, error) {
	if err := assertValidID(d.ID); err != nil {
		return false, err
	}
	if d.GroupType == "" {
		d.GroupType = GroupTypeData
	}
	if _, err := ParseGroupType(d.GroupType); err != nil {
		return false, err
	}
	existing, err := r.GetAppDataGroupDescriptor(ctx, d.ID)
	if err != nil && !errors.Is(err, ErrNotExist) {
		return false, err
	}
	merged := normalizeAppDataDescriptor(d, existing)
	next, err := merged.MarshalProjection()
	if err != nil {
		return false, err
	}
	if existing != nil {
		prev, perr := normalizeAppDataDescriptor(*existing, nil).MarshalProjection()
		if perr == nil && bytes.Equal(prev, next) {
			return false, nil
		}
	}
	full, err := merged.MarshalCanonical()
	if err != nil {
		return false, err
	}
	p := AppDataGroupPath(r.AppID, d.ID)
	if err := r.Storage.MkdirAll(ctx, path.Dir(p)); err != nil {
		return false, err
	}
	if err := r.Storage.WriteFile(ctx, p, full); err != nil {
		return false, err
	}
	if err := r.bumpVersion(ctx, p, full); err != nil {
		return false, err
	}
	return true, nil
}

// UpdateAppDataGroupDescriptor replaces one group's registered backend list.
// Name and features are preserved unless supplied in meta.
func (r *Repo) UpdateAppDataGroupDescriptor(ctx context.Context, id string, backends []BackendDescriptor, meta *AppDataGroupMeta) (bool, error) {
	existing, err := r.GetAppDataGroupDescriptor(ctx, id)
	if err != nil {
		return false, err
	}
	d := AppDataGroupDescriptor{ID: id, GroupType: GroupTypeData, Backends: backends}
	if meta != nil {
		d.Name = meta.Name
		d.Features = meta.Features
	}
	if d.Name == nil {
		d.Name = existing.Name
	}
	if d.Features == nil {
		d.Features = existing.Features
	}
	return r.SaveAppDataGroupDescriptor(ctx, d)
}

// AppDataGroupMeta carries optional descriptor fields.
type AppDataGroupMeta struct {
	Name     *string
	Features *Features
}

// UpdateAppDataGroupName rewrites the display name. The data group's own
// .meta/group.json is authoritative; when the caller holds a live DataGroup it
// must update that separately.
func (r *Repo) UpdateAppDataGroupName(ctx context.Context, id, name string) (bool, error) {
	existing, err := r.GetAppDataGroupDescriptor(ctx, id)
	if err != nil {
		return false, err
	}
	n := strings.TrimSpace(name)
	if len([]rune(n)) > 40 {
		n = string([]rune(n)[:40])
	}
	existing.Name = &n
	existing.GroupType = GroupTypeData
	return r.SaveAppDataGroupDescriptor(ctx, *existing)
}

// RemoveAppDataGroup deletes the bootstrap copy from both layouts. The data
// group's remote files are not touched.
func (r *Repo) RemoveAppDataGroup(ctx context.Context, id string) error {
	if err := assertValidID(id); err != nil {
		return err
	}
	var firstErr error
	for _, p := range []string{AppDataGroupPath(r.AppID, id), LegacyAppDataGroupPath(r.AppID, id)} {
		if _, ok, err := ReadFileOrNil(ctx, r.Storage, p); err != nil {
			return err
		} else if !ok {
			continue
		}
		if err := r.DeletePath(ctx, p); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ConfigBackends returns the config-sync group's own backend descriptors from
// .meta/backends (new layout) and .meta/backends.json (legacy single file).
func (r *Repo) ConfigBackends(ctx context.Context) ([]BackendDescriptor, error) {
	var out []BackendDescriptor
	entries, err := r.Storage.List(ctx, BackendsDir)
	if err != nil && !errors.Is(err, ErrNotExist) {
		return nil, err
	}
	for _, e := range entries {
		if e.Dir || !strings.HasSuffix(e.Path, ".json") {
			continue
		}
		data, err := r.Storage.ReadFile(ctx, e.Path)
		if err != nil {
			continue
		}
		var d BackendDescriptor
		if err := json.Unmarshal(data, &d); err != nil || d.ID == "" || d.Type == "" {
			continue
		}
		out = append(out, d)
	}
	if data, ok, err := ReadFileOrNil(ctx, r.Storage, MetaDir+"/backends.json"); err != nil {
		return nil, err
	} else if ok {
		var legacy []BackendDescriptor
		if err := json.Unmarshal(data, &legacy); err == nil {
			out = append(out, legacy...)
		}
	}
	sort.Slice(out, func(i, j int) bool { return canonjson.LessUTF16(out[i].ID, out[j].ID) })
	return out, nil
}

// CreateAppDataGroup registers a data group and returns a handle for its own
// storage. backends are written to the data group's authoritative
// .meta/backends as well as the bootstrap copy in the config group.
//
// Different groups may never point at the same remote (type+options): they
// would fight over deletions and tombstones.
func (r *Repo) CreateAppDataGroup(ctx context.Context, id string, backends []BackendDescriptor, opts CreateAppDataGroupOptions) (*DataGroup, error) {
	if err := assertValidID(id); err != nil {
		return nil, err
	}
	if _, err := r.GetAppDataGroupDescriptor(ctx, id); err == nil {
		return nil, fmt.Errorf("zenfsconfig: app data group %q already exists", id)
	} else if !errors.Is(err, ErrNotExist) {
		return nil, err
	}
	if opts.Storage == nil {
		return nil, errors.New("zenfsconfig: CreateAppDataGroup needs Storage for the data group")
	}
	if err := r.assertNoSharedRemote(ctx, backends, id); err != nil {
		return nil, err
	}
	group := NewDataGroup(id, derefString(opts.Name))
	if opts.Features != nil {
		group.Features = *opts.Features
	}
	if _, err := EnsureGroup(ctx, opts.Storage, group); err != nil {
		return nil, err
	}
	dg := &DataGroup{
		Storage: opts.Storage, AppID: r.AppID, ID: id, Group: group,
		Features: group.Features, Author: defaultString(opts.Author, r.Author), Now: r.Now,
	}
	for _, b := range backends {
		if _, err := dg.AddBackend(ctx, b); err != nil {
			return nil, err
		}
	}
	d := AppDataGroupDescriptor{ID: id, GroupType: GroupTypeData, Name: opts.Name, Features: opts.Features, Backends: dg.Backends()}
	if _, err := r.SaveAppDataGroupDescriptor(ctx, d); err != nil {
		return nil, err
	}
	return dg, nil
}

// CreateAppDataGroupOptions controls initial registration.
type CreateAppDataGroupOptions struct {
	Name     *string
	Features *Features
	Storage  Storage
	Author   string
}

func (r *Repo) assertNoSharedRemote(ctx context.Context, incoming []BackendDescriptor, ownerID string) error {
	candidates := map[string]string{}
	for _, b := range incoming {
		if err := assertValidID(b.ID); err != nil {
			return err
		}
		key, err := b.DedupKey()
		if err != nil {
			return err
		}
		candidates[key] = b.ID
	}
	if len(candidates) == 0 {
		return nil
	}
	configBackends, err := r.ConfigBackends(ctx)
	if err != nil {
		return err
	}
	for _, b := range configBackends {
		key, err := b.DedupKey()
		if err != nil {
			continue
		}
		if _, ok := candidates[key]; ok {
			return fmt.Errorf("zenfsconfig: data group %q backend %q points at the same remote as config group backend %q", ownerID, candidates[key], b.ID)
		}
	}
	groups, err := r.ListAppDataGroups(ctx)
	if err != nil {
		return err
	}
	for _, g := range groups {
		if g.ID == ownerID {
			continue
		}
		for _, b := range g.Backends {
			key, err := b.DedupKey()
			if err != nil {
				continue
			}
			if _, ok := candidates[key]; ok {
				return fmt.Errorf("zenfsconfig: data group %q backend %q points at the same remote as data group %q backend %q", ownerID, candidates[key], g.ID, b.ID)
			}
		}
	}
	return nil
}

func normalizeAppDataDescriptor(d AppDataGroupDescriptor, existing *AppDataGroupDescriptor) AppDataGroupDescriptor {
	out := d
	if out.GroupType == "" {
		out.GroupType = GroupTypeData
	}
	if len(out.Backends) == 0 && existing != nil {
		out.Backends = append([]BackendDescriptor(nil), existing.Backends...)
	}
	old := map[string]BackendDescriptor{}
	if existing != nil {
		for _, b := range existing.Backends {
			old[b.ID] = b
		}
	}
	byID := map[string]BackendDescriptor{}
	for _, b := range out.Backends {
		prev, ok := old[b.ID]
		if ok {
			b = MergeBackendDescriptor(&prev, b)
		}
		if cur, dup := byID[b.ID]; dup {
			b = MergeBackendDescriptor(&cur, b)
		}
		byID[b.ID] = b
	}
	out.Backends = out.Backends[:0]
	for _, b := range byID {
		out.Backends = append(out.Backends, b)
	}
	sort.Slice(out.Backends, func(i, j int) bool { return canonjson.LessUTF16(out.Backends[i].ID, out.Backends[j].ID) })
	out.Backends = dedupeBackendsByRemote(out.Backends)
	return out
}

func dedupeBackendsByRemote(in []BackendDescriptor) []BackendDescriptor {
	byKey := map[string]int{}
	out := make([]BackendDescriptor, 0, len(in))
	for _, b := range in {
		key, err := b.DedupKey()
		if err != nil {
			out = append(out, b)
			continue
		}
		if idx, ok := byKey[key]; ok {
			if earlierCreatedAt(b, out[idx]) {
				out[idx] = b
			}
			continue
		}
		byKey[key] = len(out)
		out = append(out, b)
	}
	return out
}

func earlierCreatedAt(a, b BackendDescriptor) bool {
	if a.CreatedAt == nil {
		return false
	}
	if b.CreatedAt == nil {
		return true
	}
	return *a.CreatedAt < *b.CreatedAt
}

// DataGroup is a data-sync group bound to its own storage. It exposes user
// files while keeping /.meta and /.config out of the app-facing namespace.
type DataGroup struct {
	Storage  Storage
	AppID    string
	ID       string
	Group    GroupDescriptor
	Features Features
	Author   string
	Now      func() time.Time
	backends []BackendDescriptor
}

// OpenDataGroup verifies that s holds a data group and returns a handle.
func OpenDataGroup(ctx context.Context, s Storage, appID, id string) (*DataGroup, error) {
	kind, group, err := ReadGroupDescriptor(ctx, s)
	if err != nil {
		return nil, err
	}
	if kind != KindData {
		return nil, fmt.Errorf("%w: expected a data group, found %q", ErrGroupTypeMismatch, kind)
	}
	if id == "" {
		id = group.ID
	}
	if id == "" {
		return nil, fmt.Errorf("%w: data group has no id", ErrInvalidID)
	}
	d := &DataGroup{
		Storage: s, AppID: appID, ID: id, Group: group,
		Features: group.Features, Author: "go-zenfs-config", Now: time.Now,
	}
	_, _ = d.reloadBackends(ctx)
	return d, nil
}

// ReadGroupDescriptor reads .meta/group.json.
func ReadGroupDescriptor(ctx context.Context, s Storage) (string, GroupDescriptor, error) {
	data, ok, err := ReadFileOrNil(ctx, s, GroupDescriptorPath)
	if err != nil {
		return "", GroupDescriptor{}, err
	}
	if !ok {
		return "", GroupDescriptor{}, ErrNotExist
	}
	var g GroupDescriptor
	if err := json.Unmarshal(data, &g); err != nil {
		return "", GroupDescriptor{}, fmt.Errorf("zenfsconfig: decode group descriptor: %w", err)
	}
	if g.SchemaVersion == 0 {
		g.SchemaVersion = 2
	}
	return g.Kind, g, nil
}

// Backends returns the authoritative backend list from .meta/backends.
func (d *DataGroup) Backends() []BackendDescriptor {
	return append([]BackendDescriptor(nil), d.backends...)
}

// reloadBackends refreshes handles lazily; it is cheap for local stores.
func (d *DataGroup) reloadBackends(ctx context.Context) ([]BackendDescriptor, error) {
	d.backends = nil
	entries, err := d.Storage.List(ctx, BackendsDir)
	if err != nil {
		if errors.Is(err, ErrNotExist) {
			d.backends = []BackendDescriptor{}
			return d.backends, nil
		}
		return nil, err
	}
	var out []BackendDescriptor
	for _, e := range entries {
		if e.Dir || !strings.HasSuffix(e.Path, ".json") {
			continue
		}
		data, err := d.Storage.ReadFile(ctx, e.Path)
		if err != nil {
			continue
		}
		var b BackendDescriptor
		if err := json.Unmarshal(data, &b); err != nil || b.ID == "" || b.Type == "" {
			continue
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return canonjson.LessUTF16(out[i].ID, out[j].ID) })
	d.backends = out
	return out, nil
}

// LoadBackends reads the authoritative backend list from storage.
func (d *DataGroup) LoadBackends(ctx context.Context) ([]BackendDescriptor, error) {
	return d.reloadBackends(ctx)
}

// AddBackend writes .meta/backends/{id}.json with canonical bytes. createdAt
// merges by min so devices do not rewrite each other's metadata.
func (d *DataGroup) AddBackend(ctx context.Context, b BackendDescriptor) (bool, error) {
	if err := assertValidID(b.ID); err != nil {
		return false, err
	}
	if b.Type == "" {
		return false, errors.New("zenfsconfig: backend type is required")
	}
	p := path.Join(BackendsDir, b.ID+".json")
	var existing *BackendDescriptor
	if data, ok, err := ReadFileOrNil(ctx, d.Storage, p); err != nil {
		return false, err
	} else if ok {
		var old BackendDescriptor
		if json.Unmarshal(data, &old) == nil && old.ID != "" {
			existing = &old
		}
	}
	merged := MergeBackendDescriptor(existing, b)
	body, err := merged.MarshalCanonical()
	if err != nil {
		return false, err
	}
	if data, ok, err := ReadFileOrNil(ctx, d.Storage, p); err != nil {
		return false, err
	} else if ok && bytes.Equal(data, body) {
		d.reloadBackends(ctx)
		return false, nil
	}
	if err := d.Storage.MkdirAll(ctx, BackendsDir); err != nil {
		return false, err
	}
	if err := d.Storage.WriteFile(ctx, p, body); err != nil {
		return false, err
	}
	// An explicit add clears an old tombstone for the same backend.
	_ = d.DeleteTombstone(ctx, p)
	_, _ = d.reloadBackends(ctx)
	return true, nil
}

// RemoveBackend tombstones and deletes .meta/backends/{id}.json.
func (d *DataGroup) RemoveBackend(ctx context.Context, id string) error {
	if err := assertValidID(id); err != nil {
		return err
	}
	p := path.Join(BackendsDir, id+".json")
	if err := d.DeleteMetaPath(ctx, p); err != nil {
		return err
	}
	_, _ = d.reloadBackends(ctx)
	return nil
}

// SaveOwnGroupDescriptor writes .meta/group.json when its canonical bytes
// changed. It does not create missing group markers (the data group may be
// registered by a config group).
func (d *DataGroup) SaveOwnGroupDescriptor(ctx context.Context) (bool, error) {
	d.Group.Kind = KindData
	d.Group.SchemaVersion = 2
	d.Group.Features = d.Features
	body, err := d.Group.MarshalCanonical()
	if err != nil {
		return false, err
	}
	if data, ok, err := ReadFileOrNil(ctx, d.Storage, GroupDescriptorPath); err != nil {
		return false, err
	} else if ok && bytes.Equal(data, body) {
		return false, nil
	}
	if err := d.Storage.MkdirAll(ctx, MetaDir); err != nil {
		return false, err
	}
	if err := d.Storage.WriteFile(ctx, GroupDescriptorPath, body); err != nil {
		return false, err
	}
	return true, nil
}

// SetName updates the authoritative display name in .meta/group.json.
func (d *DataGroup) SetName(ctx context.Context, name string) (bool, error) {
	n := strings.TrimSpace(name)
	if len([]rune(n)) > 40 {
		n = string([]rune(n)[:40])
	}
	d.Group.Name = n
	return d.SaveOwnGroupDescriptor(ctx)
}

// UserPath normalises an app-facing path and rejects the metadata namespaces.
func (d *DataGroup) UserPath(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", errors.New("zenfsconfig: empty path")
	}
	clean := path.Clean("/" + strings.TrimPrefix(p, "/"))
	if clean == MetaDir || strings.HasPrefix(clean, MetaDir+"/") {
		return "", fmt.Errorf("zenfsconfig: %q is inside the data group metadata namespace", p)
	}
	if clean == ConfigDir || strings.HasPrefix(clean, ConfigDir+"/") {
		return "", fmt.Errorf("zenfsconfig: %q is reserved for config-sync", p)
	}
	return clean, nil
}

// ReadFile reads a user file.
func (d *DataGroup) ReadFile(ctx context.Context, p string) ([]byte, error) {
	clean, err := d.UserPath(p)
	if err != nil {
		return nil, err
	}
	return d.Storage.ReadFile(ctx, clean)
}

// WriteFile writes a user file and records a version when the group enables
// versions. It returns false when the bytes are unchanged.
func (d *DataGroup) WriteFile(ctx context.Context, p string, data []byte) (bool, error) {
	clean, err := d.UserPath(p)
	if err != nil {
		return false, err
	}
	if existing, ok, err := ReadFileOrNil(ctx, d.Storage, clean); err != nil {
		return false, err
	} else if ok && bytes.Equal(existing, data) {
		return false, nil
	}
	if err := d.Storage.MkdirAll(ctx, path.Dir(clean)); err != nil {
		return false, err
	}
	if err := d.Storage.WriteFile(ctx, clean, data); err != nil {
		return false, err
	}
	_ = d.DeleteTombstone(ctx, clean)
	if err := d.recordVersion(ctx, clean, data); err != nil {
		return false, err
	}
	return true, nil
}

// DeleteFile writes a tombstone, removes the file, and drops its version record.
func (d *DataGroup) DeleteFile(ctx context.Context, p string) error {
	clean, err := d.UserPath(p)
	if err != nil {
		return err
	}
	if err := d.DeleteMetaPath(ctx, clean); err != nil {
		return err
	}
	if vp := DataVersionPath(clean); vp != "" {
		_ = d.Storage.Remove(ctx, vp)
	}
	return nil
}

// List returns the immediate children of dir.
func (d *DataGroup) List(ctx context.Context, dir string) ([]Entry, error) {
	return d.Storage.List(ctx, dir)
}

// Walk returns every file below the data group root.
func (d *DataGroup) Walk(ctx context.Context) ([]Entry, error) {
	return Walk(ctx, d.Storage, "/")
}

// ReadVersion returns the central version record for a data file.
func (d *DataGroup) ReadVersion(ctx context.Context, p string) (*VersionMeta, error) {
	vp := DataVersionPath(p)
	if vp == "" {
		return nil, nil
	}
	data, ok, err := ReadFileOrNil(ctx, d.Storage, vp)
	if err != nil || !ok {
		return nil, err
	}
	var m VersionMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, nil
	}
	if m.Hash == "" {
		return nil, nil
	}
	return &m, nil
}

func (d *DataGroup) recordVersion(ctx context.Context, p string, content []byte) error {
	if !d.Features.Versions {
		return nil
	}
	vp := DataVersionPath(p)
	if vp == "" {
		return nil
	}
	prev, err := d.ReadVersion(ctx, p)
	if err != nil {
		return err
	}
	hash := HashBytes(content)
	if prev != nil && prev.Hash == hash {
		return nil
	}
	next := VersionMeta{Version: 1, Hash: hash, Author: d.author(), Timestamp: d.now().UnixMilli()}
	if prev != nil {
		next.Version = prev.Version + 1
	}
	body, err := next.MarshalCanonical()
	if err != nil {
		return err
	}
	if err := d.Storage.MkdirAll(ctx, path.Dir(vp)); err != nil {
		return err
	}
	return d.Storage.WriteFile(ctx, vp, body)
}

func (d *DataGroup) author() string {
	if d.Author != "" {
		return d.Author
	}
	return "go-zenfs-config"
}

func (d *DataGroup) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// DeleteMetaPath tombstones a file inside the group and removes its sidecars.
// p is a raw group path (not user-facing), so it must already be cleaned.
func (d *DataGroup) DeleteMetaPath(ctx context.Context, p string) error {
	t := Tombstone{Path: p, DeletedAt: d.now().UnixMilli(), DeletedBy: d.author(), ConfirmedBy: []string{d.author()}}
	body, err := t.MarshalCanonical()
	if err != nil {
		return err
	}
	if err := d.Storage.MkdirAll(ctx, TombstonesDir); err != nil {
		return err
	}
	if err := d.Storage.WriteFile(ctx, TombstonePath(p), body); err != nil {
		return err
	}
	_ = d.Storage.Remove(ctx, p)
	for _, sp := range []string{p + ".mtime", p + ".version", p + ".version.mtime"} {
		_ = d.Storage.Remove(ctx, sp)
	}
	return nil
}

// DeleteTombstone removes a tombstone record (explicit re-add).
func (d *DataGroup) DeleteTombstone(ctx context.Context, p string) error {
	for _, tp := range []string{TombstonePath(p), LegacyTombstonePath(p)} {
		_ = d.Storage.Remove(ctx, tp)
	}
	return nil
}

func defaultString(v, fallback string) string {
	if strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
