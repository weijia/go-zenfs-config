package zenfsconfig

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Errors returned by group-type detection and Connect.
var (
	// ErrGroupTypeMismatch means the remote group is not the type the caller
	// needs (for example a data-sync group where a config-sync group was expected).
	ErrGroupTypeMismatch = errors.New("zenfsconfig: group type mismatch")
	// ErrUnknownGroupType means a group-type marker holds a value this library
	// does not recognise, so the caller must decide explicitly.
	ErrUnknownGroupType = errors.New("zenfsconfig: unknown group type")
)

// ParseGroupType validates an exact marker value. Unlike the old heuristic it
// never guesses: only "config-sync" and "data-sync" are accepted, matching
// connect.ts's detectGroupType.
func ParseGroupType(raw string) (string, error) {
	switch t := strings.TrimSpace(raw); t {
	case GroupTypeConfig, GroupTypeData:
		return t, nil
	case "":
		return "", ErrNotExist
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownGroupType, t)
	}
}

// KindForGroupType maps a marker value to a group kind.
func KindForGroupType(groupType string) string {
	if groupType == GroupTypeData {
		return KindData
	}
	return KindConfig
}

// DetectGroupType reads the group-type marker without guessing.
//
// Order matches connect.ts: /.config/group-type, then the legacy
// /.meta/group-type, then .meta/group.json's kind.
//
// found=false means no marker exists at all (a brand-new remote), so the caller
// may apply its own default. An unrecognised marker value is an error rather
// than a silent config-sync fallback.
func DetectGroupType(ctx context.Context, s Storage) (groupType string, found bool, err error) {
	for _, p := range []string{CfgGroupTypePath, MetaGroupTypePath} {
		data, ok, err := ReadFileOrNil(ctx, s, p)
		if err != nil {
			return "", false, err
		}
		if !ok {
			continue
		}
		t := strings.TrimSpace(string(data))
		if t == "" {
			continue
		}
		parsed, perr := ParseGroupType(t)
		if perr != nil {
			return "", false, fmt.Errorf("%s: %w", p, perr)
		}
		return parsed, true, nil
	}
	if kind, _, err := ReadGroupDescriptor(ctx, s); err != nil {
		if !errors.Is(err, ErrNotExist) {
			return "", false, err
		}
	} else if kind != "" {
		switch kind {
		case KindConfig:
			return GroupTypeConfig, true, nil
		case KindData:
			return GroupTypeData, true, nil
		default:
			return "", false, fmt.Errorf("%s: %w: kind %q", GroupDescriptorPath, ErrUnknownGroupType, kind)
		}
	}
	return "", false, nil
}

// ResolveGroupType returns the effective group type, applying fallback when the
// remote carries no marker (JS: options.groupType, then "config-sync").
func ResolveGroupType(ctx context.Context, s Storage, fallback string) (string, error) {
	if fallback == "" {
		fallback = GroupTypeConfig
	}
	if _, err := ParseGroupType(fallback); err != nil {
		return "", err
	}
	detected, found, err := DetectGroupType(ctx, s)
	if err != nil {
		return "", err
	}
	if found {
		return detected, nil
	}
	return fallback, nil
}

// ConnectOptions configures Connect.
type ConnectOptions struct {
	// AppID is the application namespace used for config paths and app-data
	// group registration.
	AppID string
	// NodeID is recorded in version records, e.g. "openlist".
	NodeID string
	// Storage is the remote group backend. Nil means local-only operation,
	// which always yields a config-sync repo (JS semantics).
	Storage Storage
	// GroupType overrides detection when the remote carries no marker.
	GroupType string
	// ExpectType rejects the connection when the detected type differs.
	ExpectType string
	// ConfigStorage is the config-sync group's storage. For a data-sync remote
	// the config group is local-only, so this is usually a separate local store.
	// Defaults to Storage for config-sync remotes.
	ConfigStorage Storage
	// DataStorage is the data group's own storage for a data-sync remote.
	// Defaults to Storage.
	DataStorage Storage
	// DataGroupID is the registered data group id; defaults to "default".
	DataGroupID string
	// DataGroupName is the display name of a newly created data group.
	DataGroupName string
	// Backend registers the data group's primary backend. For data-sync remotes
	// this is normally {ID: type+"-primary", Type: type, Options: options}.
	Backend *BackendDescriptor
	// Author is recorded in version records and tombstones.
	Author string
	// Now is injectable for tests.
	Now func() time.Time
}

// ConnectResult is either a config-sync repo or a data-sync group.
type ConnectResult struct {
	// GroupType is the effective type: "config-sync" or "data-sync".
	GroupType string
	// Repo is non-nil for config-sync groups (and for data-sync groups, as the
	// local-only host that registers the data group).
	Repo *Repo
	// Data is non-nil for data-sync groups.
	Data *DataGroup
}

// IsConfig reports whether the result is a config-sync connection.
func (c ConnectResult) IsConfig() bool { return c.GroupType == GroupTypeConfig }

// IsData reports whether the result is a data-sync connection.
func (c ConnectResult) IsData() bool { return c.GroupType == GroupTypeData }

// Connect auto-detects the group type and returns the matching handle.
//
// config-sync: returns a Repo bound to the remote.
// data-sync: returns a Repo on the (local-only) config group plus the data
// group handle, registering a default data group and its primary backend when
// the config group has no descriptor for it yet.
func Connect(ctx context.Context, opts ConnectOptions) (ConnectResult, error) {
	if opts.AppID == "" {
		return ConnectResult{}, errors.New("zenfsconfig: Connect needs AppID")
	}
	if opts.DataGroupID == "" {
		opts.DataGroupID = "default"
	}
	if opts.Author == "" {
		opts.Author = "go-zenfs-config"
	}

	groupType := opts.GroupType
	if groupType != "" {
		if _, err := ParseGroupType(groupType); err != nil {
			return ConnectResult{}, err
		}
	} else if opts.Storage != nil {
		t, err := ResolveGroupType(ctx, opts.Storage, opts.GroupType)
		if err != nil {
			return ConnectResult{}, err
		}
		groupType = t
	} else {
		groupType = GroupTypeConfig
	}
	if opts.ExpectType != "" && opts.ExpectType != groupType {
		return ConnectResult{}, fmt.Errorf("%w: expected %s, found %s", ErrGroupTypeMismatch, opts.ExpectType, groupType)
	}

	if groupType == GroupTypeConfig {
		store := opts.Storage
		if opts.ConfigStorage != nil {
			store = opts.ConfigStorage
		}
		if store == nil {
			return ConnectResult{}, errors.New("zenfsconfig: config-sync connection needs Storage")
		}
		repo := &Repo{Storage: store, AppID: opts.AppID, Group: GroupDescriptor{Kind: KindConfig, ID: "config"}, Author: opts.Author, Now: opts.Now}
		if g, err := readGroupOrDefault(ctx, store); err == nil {
			repo.Group = g
		}
		return ConnectResult{GroupType: GroupTypeConfig, Repo: repo}, nil
	}

	// data-sync: the config group is the host that registers data groups. When
	// the remote itself is the data group, the config group is local-only.
	configStore := opts.ConfigStorage
	if configStore == nil && opts.DataStorage == nil {
		configStore = opts.Storage
	}
	if configStore == nil {
		return ConnectResult{}, errors.New("zenfsconfig: data-sync connection needs ConfigStorage for the registering config group")
	}
	repo := &Repo{Storage: configStore, AppID: opts.AppID, Group: GroupDescriptor{Kind: KindConfig, ID: "config"}, Author: opts.Author, Now: opts.Now}
	if g, err := readGroupOrDefault(ctx, configStore); err == nil {
		repo.Group = g
	}

	dataStore := opts.DataStorage
	if dataStore == nil {
		dataStore = opts.Storage
	}
	if dataStore == nil {
		return ConnectResult{}, errors.New("zenfsconfig: data-sync connection needs DataStorage")
	}
	if err := repo.EnsureAppDataGroup(ctx, opts.DataGroupID, dataStore, opts.DataGroupName, opts.Backend); err != nil {
		return ConnectResult{}, err
	}
	group, err := OpenDataGroup(ctx, dataStore, opts.AppID, opts.DataGroupID)
	if err != nil {
		return ConnectResult{}, err
	}
	group.Author = opts.Author
	group.Now = opts.Now
	return ConnectResult{GroupType: GroupTypeData, Repo: repo, Data: group}, nil
}

// EnsureAppDataGroup makes a data group discoverable from the config group.
// It creates the data group's own group descriptor when missing, registers the
// primary backend, and writes the bootstrap copy idempotently. An existing
// registration is left untouched apart from missing pieces.
func (r *Repo) EnsureAppDataGroup(ctx context.Context, id string, storage Storage, name string, backend *BackendDescriptor) error {
	if err := assertValidID(id); err != nil {
		return err
	}
	if storage == nil {
		return errors.New("zenfsconfig: EnsureAppDataGroup needs the data group Storage")
	}
	existing, err := r.GetAppDataGroupDescriptor(ctx, id)
	if err != nil && !errors.Is(err, ErrNotExist) {
		return err
	}
	created := false
	if _, _, gerr := ReadGroupDescriptor(ctx, storage); gerr != nil {
		if !errors.Is(gerr, ErrNotExist) {
			return gerr
		}
		g := NewDataGroup(id, name)
		if _, err := EnsureGroup(ctx, storage, g); err != nil {
			return err
		}
		created = true
	}
	_ = created
	dg, err := OpenDataGroup(ctx, storage, r.AppID, id)
	if err != nil {
		return err
	}
	dg.Author = r.Author
	dg.Now = r.Now
	if backend != nil {
		if _, err := dg.AddBackend(ctx, *backend); err != nil {
			return err
		}
	}
	descriptor := AppDataGroupDescriptor{ID: id, GroupType: GroupTypeData, Backends: dg.Backends()}
	if existing != nil {
		descriptor.Name = existing.Name
		descriptor.Features = existing.Features
		descriptor.Backends = existing.Backends
		// An existing registration wins; only fill in backends it lacks.
		known := map[string]bool{}
		for _, b := range descriptor.Backends {
			known[b.ID] = true
		}
		for _, b := range dg.Backends() {
			if !known[b.ID] {
				descriptor.Backends = append(descriptor.Backends, b)
			}
		}
	} else if name != "" {
		descriptor.Name = &name
	}
	if len(descriptor.Backends) > 0 {
		if err := r.assertNoSharedRemote(ctx, descriptor.Backends, id); err != nil {
			return err
		}
	}
	_, err = r.SaveAppDataGroupDescriptor(ctx, descriptor)
	return err
}

func readGroupOrDefault(ctx context.Context, s Storage) (GroupDescriptor, error) {
	_, g, err := ReadGroupDescriptor(ctx, s)
	if err != nil {
		if errors.Is(err, ErrNotExist) {
			return GroupDescriptor{Kind: KindConfig, ID: "config"}, nil
		}
		return GroupDescriptor{}, err
	}
	if g.Kind == "" {
		g.Kind = KindConfig
	}
	return g, nil
}
