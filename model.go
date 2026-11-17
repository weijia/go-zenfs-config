package zenfsconfig

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/weijia/go-zenfs-config/canonjson"
)

// Group kinds.
const (
	KindConfig = "config"
	KindData   = "data"
)

// Group type markers stored in /.config/group-type and /.meta/group-type.
const (
	GroupTypeConfig = "config-sync"
	GroupTypeData   = "data-sync"
)

// Features mirrors the five capability flags of a zen-fs-config group.
type Features struct {
	Tombstones bool `json:"tombstones"`
	Versions   bool `json:"versions"`
	Conflicts  bool `json:"conflicts"`
	ConfigHost bool `json:"configHost"`
	AppData    bool `json:"appData"`
}

// ConfigFeatures is the default feature set for a config-sync group.
func ConfigFeatures() Features {
	return Features{Tombstones: true, Versions: true, Conflicts: true, ConfigHost: true, AppData: false}
}

// DataFeatures is the default feature set for a data-sync group.
func DataFeatures() Features {
	return Features{Tombstones: true, Versions: false, Conflicts: false, ConfigHost: false, AppData: true}
}

// GroupDescriptor is /.meta/group.json.
type GroupDescriptor struct {
	SchemaVersion int      `json:"schemaVersion"`
	Kind          string   `json:"kind"`
	ID            string   `json:"id"`
	Name          string   `json:"name,omitempty"` // omitted when empty
	Features      Features `json:"features"`
}

// NewConfigGroup returns the descriptor a config-sync group bootstraps with.
func NewConfigGroup(id, name string) GroupDescriptor {
	return GroupDescriptor{SchemaVersion: 2, Kind: KindConfig, ID: id, Name: name, Features: ConfigFeatures()}
}

// NewDataGroup returns the descriptor a data-sync group bootstraps with.
func NewDataGroup(id, name string) GroupDescriptor {
	return GroupDescriptor{SchemaVersion: 2, Kind: KindData, ID: id, Name: name, Features: DataFeatures()}
}

// MarshalCanonical returns bytes identical to serializeGroupDescriptor().
func (g GroupDescriptor) MarshalCanonical() ([]byte, error) {
	o := canonjson.Object{
		{K: "schemaVersion", V: g.SchemaVersion},
		{K: "kind", V: g.Kind},
		{K: "id", V: g.ID},
	}
	if g.Name != "" {
		o = append(o, canonjson.Pair{K: "name", V: g.Name})
	}
	o = append(o, canonjson.Pair{K: "features", V: featuresObject(g.Features)})
	return canonjson.Marshal(o)
}

func featuresObject(f Features) canonjson.Object {
	return canonjson.Object{
		{K: "tombstones", V: f.Tombstones},
		{K: "versions", V: f.Versions},
		{K: "conflicts", V: f.Conflicts},
		{K: "configHost", V: f.ConfigHost},
		{K: "appData", V: f.AppData},
	}
}

// BackendDescriptor is /.meta/backends/{id}.json (and an entry inside an
// app-data-group bootstrap descriptor).
//
// Optional fields are pointers so that "absent" is distinguishable from
// "empty"; JS drops undefined fields entirely instead of writing null.
type BackendDescriptor struct {
	ID               string         `json:"id"`
	Type             string         `json:"type"`
	Options          map[string]any `json:"options"`
	AccountBackendID *string        `json:"accountBackendId,omitempty"`
	Description      *string        `json:"description,omitempty"`
	CreatedAt        *int64         `json:"createdAt,omitempty"`
}

// MarshalCanonical returns bytes identical to serializeBackendDescriptor().
func (b BackendDescriptor) MarshalCanonical() ([]byte, error) {
	opts, err := optionsObject(b.Options)
	if err != nil {
		return nil, err
	}
	o := canonjson.Object{
		{K: "id", V: b.ID},
		{K: "type", V: b.Type},
		{K: "options", V: opts},
	}
	if b.AccountBackendID != nil {
		o = append(o, canonjson.Pair{K: "accountBackendId", V: *b.AccountBackendID})
	}
	if b.Description != nil {
		o = append(o, canonjson.Pair{K: "description", V: *b.Description})
	}
	if b.CreatedAt != nil {
		o = append(o, canonjson.Pair{K: "createdAt", V: *b.CreatedAt})
	}
	return canonjson.Marshal(o)
}

// StripCreatedAt returns a copy without CreatedAt, used for projection
// comparison ("does this backend list need to be written?").
func (b BackendDescriptor) StripCreatedAt() BackendDescriptor {
	c := b
	c.CreatedAt = nil
	return c
}

func optionsObject(m map[string]any) (canonjson.Object, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return canonjson.LessUTF16(keys[i], keys[j]) })
	o := make(canonjson.Object, 0, len(keys))
	for _, k := range keys {
		cv, err := canonValue(m[k], k)
		if err != nil {
			return nil, err
		}
		o = append(o, canonjson.Pair{K: k, V: cv})
	}
	return o, nil
}

// canonValue converts descriptor field values into canonjson values, sorting
// nested object keys by UTF-16 code units (the "options and other free
// objects" rule).
func canonValue(v any, key string) (any, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i, nil
		}
		f, err := x.Float64()
		if err != nil {
			return nil, fmt.Errorf("zenfsconfig: bad number %q: %w", x.String(), err)
		}
		return f, nil
	case string, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return x, nil
	case map[string]any:
		return optionsObject(x)
	case []any:
		out := make(canonjson.Array, 0, len(x))
		for i, item := range x {
			cv, err := canonValue(item, fmt.Sprintf("%s[%d]", key, i))
			if err != nil {
				return nil, err
			}
			out = append(out, cv)
		}
		return out, nil
	case []string:
		out := make(canonjson.Array, 0, len(x))
		for _, s := range x {
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("zenfsconfig: unsupported descriptor value for %q: %T", key, v)
	}
}

// AppDataGroupDescriptor is /.config/app-data-groups/{appId}/{id}.json.
type AppDataGroupDescriptor struct {
	ID        string              `json:"id"`
	GroupType string              `json:"groupType"`
	Name      *string             `json:"name,omitempty"`
	Features  *Features           `json:"features,omitempty"`
	Backends  []BackendDescriptor `json:"backends"`
}

// MarshalProjection returns the descriptor bytes used for "needs write?"
// comparison: canonical serialization with every backend's createdAt removed.
// Timestamps are merged separately (min wins) and never force a rewrite.
func (d AppDataGroupDescriptor) MarshalProjection() ([]byte, error) {
	c := d
	c.Backends = make([]BackendDescriptor, len(d.Backends))
	for i, b := range d.Backends {
		c.Backends[i] = b.StripCreatedAt()
	}
	return c.MarshalCanonical()
}

// DedupKey identifies the same remote backend across groups: type plus the
// canonical form of its options. Two groups must never point at the same remote.
func (b BackendDescriptor) DedupKey() (string, error) {
	opts, err := optionsObject(b.Options)
	if err != nil {
		return "", err
	}
	data, err := canonjson.Marshal(opts)
	if err != nil {
		return "", err
	}
	return b.Type + "|" + string(data), nil
}

// MergeBackendDescriptor merges existing with incoming: the incoming fields win,
// but createdAt is the earlier of the two so devices never rewrite each other's
// metadata just because their clocks differ.
func MergeBackendDescriptor(existing *BackendDescriptor, incoming BackendDescriptor) BackendDescriptor {
	out := incoming
	if existing == nil {
		return out
	}
	if existing.CreatedAt != nil && (out.CreatedAt == nil || *existing.CreatedAt < *out.CreatedAt) {
		v := *existing.CreatedAt
		out.CreatedAt = &v
	}
	return out
}

// MarshalCanonical returns bytes identical to
// serializeAppDataGroupDescriptor(): backends sorted by id, createdAt kept if
// present (callers that need the projection form strip it beforehand).
func (d AppDataGroupDescriptor) MarshalCanonical() ([]byte, error) {
	o := canonjson.Object{
		{K: "id", V: d.ID},
		{K: "groupType", V: d.GroupType},
	}
	if d.Name != nil {
		o = append(o, canonjson.Pair{K: "name", V: *d.Name})
	}
	if d.Features != nil {
		o = append(o, canonjson.Pair{K: "features", V: featuresObject(*d.Features)})
	}
	backends, err := d.backendsArray(d.Backends)
	if err != nil {
		return nil, err
	}
	o = append(o, canonjson.Pair{K: "backends", V: backends})
	return canonjson.Marshal(o)
}

func (d AppDataGroupDescriptor) backendsArray(list []BackendDescriptor) (canonjson.Array, error) {
	sorted := make([]BackendDescriptor, len(list))
	copy(sorted, list)
	sort.SliceStable(sorted, func(i, j int) bool { return canonjson.LessUTF16(sorted[i].ID, sorted[j].ID) })
	out := make(canonjson.Array, 0, len(sorted))
	for _, b := range sorted {
		opts, err := optionsObject(b.Options)
		if err != nil {
			return nil, err
		}
		o := canonjson.Object{
			{K: "id", V: b.ID},
			{K: "type", V: b.Type},
			{K: "options", V: opts},
		}
		if b.AccountBackendID != nil {
			o = append(o, canonjson.Pair{K: "accountBackendId", V: *b.AccountBackendID})
		}
		if b.Description != nil {
			o = append(o, canonjson.Pair{K: "description", V: *b.Description})
		}
		if b.CreatedAt != nil {
			o = append(o, canonjson.Pair{K: "createdAt", V: *b.CreatedAt})
		}
		out = append(out, o)
	}
	return out, nil
}

// VersionMeta is the payload of a "<file>.version" sidecar, and of
// /.meta/versions/{encoded}.json for data groups.
type VersionMeta struct {
	Version   int64  `json:"version"`
	Hash      string `json:"hash"` // "sha256:<hex>"
	Author    string `json:"author"`
	Timestamp int64  `json:"timestamp"`
}

// MarshalCanonical returns bytes identical to writeVersion(): JSON.stringify
// with 2-space indentation and no trailing newline, in fixed field order.
func (v VersionMeta) MarshalCanonical() ([]byte, error) {
	return canonjson.Marshal(canonjson.Object{
		{K: "version", V: v.Version},
		{K: "hash", V: v.Hash},
		{K: "author", V: v.Author},
		{K: "timestamp", V: v.Timestamp},
	})
}
