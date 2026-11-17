package zenfsconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/weijia/go-zenfs-config/canonjson"
)

// Repo is a single application's view of a config-sync group.
type Repo struct {
	Storage Storage
	AppID   string
	Group   GroupDescriptor
	// Author is written into version sidecars, e.g. "openlist/node-1".
	Author string
	// Now is injectable for tests; defaults to time.Now.
	Now func() time.Time
}

// Open detects the group type and returns a repo bound to appID.
//
// Detection order matches connect.ts:
//
//	/.config/group-type -> /.meta/group-type -> /.meta/group.json kind -> config
//
// A missing group descriptor is not an error: the caller can call
// EnsureGroup to bootstrap one.
func Open(ctx context.Context, s Storage, appID string) (*Repo, error) {
	kind, err := ProbeGroupKind(ctx, s)
	if err != nil {
		return nil, err
	}
	return &Repo{Storage: s, AppID: appID, Group: GroupDescriptor{Kind: kind}, Author: "go-zenfs-config", Now: time.Now}, nil
}

// ProbeGroupKind returns KindConfig, KindData, or "" when the group carries no
// marker at all. Marker values are matched exactly; an unrecognised value is
// an error instead of being guessed as config-sync.
func ProbeGroupKind(ctx context.Context, s Storage) (string, error) {
	if data, ok, err := ReadFileOrNil(ctx, s, CfgGroupTypePath); err != nil {
		return "", err
	} else if ok {
		if t := strings.TrimSpace(string(data)); t != "" {
			groupType, perr := ParseGroupType(t)
			if perr != nil {
				return "", fmt.Errorf("%s: %w", CfgGroupTypePath, perr)
			}
			return KindForGroupType(groupType), nil
		}
	}
	if data, ok, err := ReadFileOrNil(ctx, s, MetaGroupTypePath); err != nil {
		return "", err
	} else if ok {
		if t := strings.TrimSpace(string(data)); t != "" {
			groupType, perr := ParseGroupType(t)
			if perr != nil {
				return "", fmt.Errorf("%s: %w", MetaGroupTypePath, perr)
			}
			return KindForGroupType(groupType), nil
		}
	}
	if kind, _, err := ReadGroupDescriptor(ctx, s); err != nil {
		if errors.Is(err, ErrNotExist) {
			return "", nil
		}
		return "", err
	} else {
		switch kind {
		case KindConfig, KindData:
			return kind, nil
		default:
			return "", fmt.Errorf("%s: %w: kind %q", GroupDescriptorPath, ErrUnknownGroupType, kind)
		}
	}
}

// EnsureGroup writes .meta/group.json and both group-type markers when the
// group descriptor is missing. It never overwrites an existing descriptor.
func EnsureGroup(ctx context.Context, s Storage, g GroupDescriptor) (bool, error) {
	if data, ok, err := ReadFileOrNil(ctx, s, GroupDescriptorPath); err != nil {
		return false, err
	} else if ok && len(data) > 0 {
		return false, nil
	}
	if g.SchemaVersion == 0 {
		g.SchemaVersion = 2
	}
	bytesGroup, err := g.MarshalCanonical()
	if err != nil {
		return false, err
	}
	if err := s.MkdirAll(ctx, ConfigDir); err != nil {
		return false, err
	}
	if err := s.WriteFile(ctx, GroupDescriptorPath, bytesGroup); err != nil {
		return false, err
	}
	groupType := GroupTypeConfig
	if g.Kind == KindData {
		groupType = GroupTypeData
	}
	for _, p := range []string{MetaGroupTypePath, CfgGroupTypePath} {
		if _, ok, err := ReadFileOrNil(ctx, s, p); err != nil {
			return false, err
		} else if ok {
			continue
		}
		if err := s.WriteFile(ctx, p, []byte(groupType)); err != nil {
			return false, err
		}
	}
	return true, nil
}

// ConfigPath maps a config key to /{appID}/... path.
//
//	"site/title"   -> /myapp/site/title.json
//	"readme.md"    -> /myapp/readme.md
func (r *Repo) ConfigPath(key string) string {
	return "/" + r.AppID + configKeyToFilePath(key)
}

// NodeConfigPath maps a node-local key to /nodes/{nodeID}/... (no app prefix).
func (r *Repo) NodeConfigPath(nodeID, key string) string {
	return "/nodes/" + nodeID + configKeyToFilePath(key)
}

func configKeyToFilePath(key string) string {
	p := key
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if ext(p) != "" {
		return p
	}
	if strings.HasSuffix(p, "/") {
		return p
	}
	return p + ".json"
}

func ext(p string) string {
	lastSlash := strings.LastIndex(p, "/")
	lastDot := strings.LastIndex(p, ".")
	if lastDot > lastSlash && lastDot < len(p)-1 {
		return p[lastDot:]
	}
	return ""
}

// ReadConfig returns the raw bytes of a config file.
func (r *Repo) ReadConfig(ctx context.Context, key string) ([]byte, error) {
	return r.Storage.ReadFile(ctx, r.ConfigPath(key))
}

// ReadConfigValue decodes a config file into out.
func (r *Repo) ReadConfigValue(ctx context.Context, key string, out any) error {
	data, err := r.ReadConfig(ctx, key)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

// SetConfig writes value if its canonical bytes differ from what is stored.
//
// Ordering follows version.ts: content first, then the version sidecar. The
// version is only bumped when the content hash changes.
func (r *Repo) SetConfig(ctx context.Context, key string, value any) (bool, error) {
	p := r.ConfigPath(key)
	data, err := MarshalConfigValue(value)
	if err != nil {
		return false, err
	}
	existing, ok, err := ReadFileOrNil(ctx, r.Storage, p)
	if err != nil {
		return false, err
	}
	if ok {
		if bytes.Equal(existing, data) || jsonSemanticallyEqual(existing, data) {
			return false, nil
		}
	}
	if err := r.Storage.MkdirAll(ctx, path.Dir(p)); err != nil {
		return false, err
	}
	if err := r.Storage.WriteFile(ctx, p, data); err != nil {
		return false, err
	}
	if err := r.bumpVersion(ctx, p, data); err != nil {
		return false, err
	}
	return true, nil
}

// DeleteConfig records a tombstone, then removes the content and its sidecars.
func (r *Repo) DeleteConfig(ctx context.Context, key string) error {
	p := r.ConfigPath(key)
	if err := r.DeletePath(ctx, p); err != nil {
		return err
	}
	return nil
}

// DeletePath applies the tombstone-then-remove order for any group file.
func (r *Repo) DeletePath(ctx context.Context, p string) error {
	ts := Tombstone{Path: p, DeletedAt: r.now().UnixMilli(), DeletedBy: r.Author, ConfirmedBy: []string{r.Author}}
	body, err := ts.MarshalCanonical()
	if err != nil {
		return err
	}
	if err := r.Storage.MkdirAll(ctx, TombstonesDir); err != nil {
		return err
	}
	if err := r.Storage.WriteFile(ctx, TombstonePath(p), body); err != nil {
		return err
	}
	_ = r.Storage.Remove(ctx, p)
	for _, sp := range []string{p + ".version", p + ".version.mtime", p + ".mtime"} {
		_ = r.Storage.Remove(ctx, sp)
	}
	return nil
}

func (r *Repo) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// bumpVersion writes or increments the sidecar for p. A missing .version is
// created with version=1; an unchanged hash keeps the old record untouched.
func (r *Repo) bumpVersion(ctx context.Context, p string, content []byte) error {
	vp := VersionPathFor(p)
	if vp == "" {
		return nil
	}
	meta, err := r.readVersion(ctx, p)
	if err != nil {
		return err
	}
	hash := HashBytes(content)
	if meta != nil && meta.Hash == hash {
		return nil
	}
	next := VersionMeta{Version: 1, Hash: hash, Author: r.Author, Timestamp: r.now().UnixMilli()}
	if meta != nil {
		next.Version = meta.Version + 1
	}
	body, err := next.MarshalCanonical()
	if err != nil {
		return err
	}
	if err := r.Storage.MkdirAll(ctx, path.Dir(vp)); err != nil {
		return err
	}
	return r.Storage.WriteFile(ctx, vp, body)
}

// ReadVersion returns the sidecar for p, falling back to the legacy dot name.
func (r *Repo) ReadVersion(ctx context.Context, p string) (*VersionMeta, error) {
	return r.readVersion(ctx, p)
}

func (r *Repo) readVersion(ctx context.Context, p string) (*VersionMeta, error) {
	candidates := []string{VersionPathFor(p)}
	if legacy := LegacyVersionPathFor(p); legacy != "" {
		candidates = append(candidates, legacy)
	}
	for _, vp := range candidates {
		if vp == "" {
			continue
		}
		data, ok, err := ReadFileOrNil(ctx, r.Storage, vp)
		if err != nil {
			return nil, err
		} else if !ok {
			continue
		}
		var m VersionMeta
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		if m.Hash == "" {
			continue
		}
		return &m, nil
	}
	return nil, nil
}

// Tombstones returns the union of the canonical and legacy tombstone
// directories, keyed by path. Canonical entries win; corrupt files are
// recovered conservatively.
func (r *Repo) Tombstones(ctx context.Context) (map[string]Tombstone, error) {
	out := map[string]Tombstone{}
	for _, dir := range []string{TombstonesDir, LegacyDeletedDir} {
		entries, err := r.Storage.List(ctx, dir)
		if err != nil {
			if errors.Is(err, ErrNotExist) {
				continue
			}
			return nil, err
		}
		for _, e := range entries {
			if e.Dir {
				continue
			}
			data, err := r.Storage.ReadFile(ctx, e.Path)
			if err != nil {
				continue
			}
			parsed, _ := ParseTombstoneFile(data, path.Base(e.Path))
			for _, t := range parsed {
				if old, ok := out[t.Path]; !ok || t.DeletedAt > old.DeletedAt {
					out[t.Path] = t
				}
			}
		}
	}
	return out, nil
}

// TombstoneAt reports whether p is deleted and how strong the record is.
func TombstoneAt(ctx context.Context, s Storage, p string) (*Tombstone, error) {
	for _, tp := range []string{TombstonePath(p), LegacyTombstonePath(p)} {
		data, ok, err := ReadFileOrNil(ctx, s, tp)
		if err != nil {
			return nil, err
		} else if !ok {
			continue
		}
		parsed, _ := ParseTombstoneFile(data, path.Base(tp))
		for _, t := range parsed {
			if t.Path == p {
				return &t, nil
			}
		}
	}
	return nil, nil
}

// MarshalConfigValue renders a config value the way the default JSON
// serializer does: 2-space indent, no trailing newline. Keys are emitted in
// UTF-16 order, which is stable and survives JS re-serialisation.
func MarshalConfigValue(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, err
	}
	conv, err := canonFromJSON(generic)
	if err != nil {
		return nil, err
	}
	return canonjson.Marshal(conv)
}

func canonFromJSON(v any) (any, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		return optionsObject(x)
	case []any:
		out := make(canonjson.Array, 0, len(x))
		for _, item := range x {
			cv, err := canonFromJSON(item)
			if err != nil {
				return nil, err
			}
			out = append(out, cv)
		}
		return out, nil
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i, nil
		}
		f, err := x.Float64()
		if err != nil {
			return nil, err
		}
		return f, nil
	default:
		return x, nil
	}
}

func jsonSemanticallyEqual(a, b []byte) bool {
	var va, vb any
	if err := decodeUseNumber(a, &va); err != nil {
		return false
	}
	if err := decodeUseNumber(b, &vb); err != nil {
		return false
	}
	return fmt.Sprint(va) == fmt.Sprint(vb)
}

func decodeUseNumber(data []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		// Trailing content is tolerated: only the first JSON value is compared.
		return nil
	}
	return nil
}
