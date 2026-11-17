package zenfsconfig

import (
	"bytes"
	"context"
	"errors"
	"path"
	"strings"
	"time"
)

// MirrorOptions narrows what a mirror run touches. Empty Prefixes means the
// whole tree (meta runtime directories are always excluded).
type MirrorOptions struct {
	Prefixes []string
	// Author is used when a version sidecar has to be created on the target.
	Author string
}

// MirrorStats reports what a Push/Pull changed.
type MirrorStats struct {
	Written int
	Deleted int
	Skipped int
}

// runtimeMetaDirs are never mirrored: they are per-device state or backend
// bookkeeping, not user config.
var runtimeMetaDirs = []string{
	TombstonesDir,
	LegacyDeletedDir,
	"/.meta/conflicts",
	"/.meta/.conflicts",
	VersionsDir,
}

func excluded(p string) bool {
	for _, d := range runtimeMetaDirs {
		if p == d || strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	base := path.Base(p)
	if strings.HasSuffix(base, ".mtime") {
		return true
	}
	if p == KeepPath {
		return true
	}
	return false
}

func inScope(p string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, pre := range prefixes {
		pre = strings.TrimSuffix(pre, "/")
		if p == pre || strings.HasPrefix(p, pre+"/") {
			return true
		}
	}
	return false
}

// Push mirrors src onto dst (src wins). Deletions are propagated as
// tombstones, except that a dst file newer than the tombstone's
// deletedAt + RECREATE_AFTER_DELETE_SKEW_MS is treated as re-created and
// kept.
func Push(ctx context.Context, src, dst Storage, opts MirrorOptions) (MirrorStats, error) {
	return mirror(ctx, src, dst, opts)
}

// Pull is Push with the argument order reversed.
func Pull(ctx context.Context, src, dst Storage, opts MirrorOptions) (MirrorStats, error) {
	return mirror(ctx, src, dst, opts)
}

func mirror(ctx context.Context, src, dst Storage, opts MirrorOptions) (MirrorStats, error) {
	var st MirrorStats
	srcFiles, err := Walk(ctx, src, "/")
	if err != nil {
		return st, err
	}
	dstFiles, err := Walk(ctx, dst, "/")
	if err != nil {
		return st, err
	}
	srcSet := map[string]Entry{}
	for _, e := range srcFiles {
		if excluded(e.Path) || !inScope(e.Path, opts.Prefixes) {
			continue
		}
		srcSet[e.Path] = e
	}
	dstSet := map[string]Entry{}
	for _, e := range dstFiles {
		if excluded(e.Path) || !inScope(e.Path, opts.Prefixes) {
			continue
		}
		dstSet[e.Path] = e
	}

	// 1. Copy files that are new or different.
	for p := range srcSet {
		data, err := src.ReadFile(ctx, p)
		if err != nil {
			return st, err
		}
		if _, ok := dstSet[p]; ok {
			existing, err := dst.ReadFile(ctx, p)
			if err == nil && bytes.Equal(existing, data) {
				st.Skipped++
				continue
			}
			if err != nil && !errors.Is(err, ErrNotExist) {
				return st, err
			}
		}
		if err := dst.WriteFile(ctx, p, data); err != nil {
			return st, err
		}
		if err := copyVersion(ctx, src, dst, p, data, opts.Author); err != nil {
			return st, err
		}
		st.Written++
	}

	// 2. Propagate deletions for dst-only paths (always as a tombstone).
	tombstones, err := collectTombstones(ctx, src)
	if err != nil {
		return st, err
	}
	for p, de := range dstSet {
		if _, ok := srcSet[p]; ok {
			continue
		}
		if ts, ok := tombstones[p]; ok && de.MTime.UnixMilli() > ts.DeletedAt+RECREATEAfterDeleteSkewMS {
			// The target re-created the file after the deletion: keep it and
			// retire the tombstone instead of deleting user data.
			_ = dst.Remove(ctx, TombstonePath(p))
			_ = dst.Remove(ctx, LegacyTombstonePath(p))
			continue
		}
		ts := tombstones[p]
		if ts.Path == "" {
			ts = Tombstone{Path: p, DeletedAt: nowMillis(), DeletedBy: opts.Author, ConfirmedBy: []string{}}
		}
		if err := writeTombstone(ctx, dst, ts); err != nil {
			return st, err
		}
		if err := dst.Remove(ctx, p); err != nil && !errors.Is(err, ErrNotExist) {
			return st, err
		}
		st.Deleted++
	}

	return st, nil
}

const (
	// RECREATEAfterDeleteSkewMS matches RECREATE_AFTER_DELETE_SKEW_MS in
	// group-meta.ts: a file written this soon after a deletion is treated as
	// part of the same delete/recreate race.
	RECREATEAfterDeleteSkewMS = 3000
)

func nowMillis() int64 { return time.Now().UnixMilli() }

func collectTombstones(ctx context.Context, s Storage) (map[string]Tombstone, error) {
	out := map[string]Tombstone{}
	for _, dir := range []string{TombstonesDir, LegacyDeletedDir} {
		entries, err := s.List(ctx, dir)
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
			data, err := s.ReadFile(ctx, e.Path)
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

func writeTombstone(ctx context.Context, dst Storage, ts Tombstone) error {
	body, err := ts.MarshalCanonical()
	if err != nil {
		return err
	}
	if err := dst.MkdirAll(ctx, TombstonesDir); err != nil {
		return err
	}
	return dst.WriteFile(ctx, TombstonePath(ts.Path), body)
}

// copyVersion mirrors the version sidecar of a copied file. When the source
// has none, a fresh version 1 record is written so that JS clients see a
// consistent history.
func copyVersion(ctx context.Context, src, dst Storage, p string, data []byte, author string) error {
	vp := VersionPathFor(p)
	if vp == "" {
		return nil
	}
	if v, ok, err := ReadFileOrNil(ctx, src, vp); err != nil {
		return err
	} else if ok {
		return dst.WriteFile(ctx, vp, v)
	}
	meta := VersionMeta{Version: 1, Hash: HashBytes(data), Author: author, Timestamp: nowMillis()}
	if meta.Author == "" {
		meta.Author = "go-zenfs-config"
	}
	body, err := meta.MarshalCanonical()
	if err != nil {
		return err
	}
	return dst.WriteFile(ctx, vp, body)
}
