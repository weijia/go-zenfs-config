package zenfsconfig

import (
	"context"
	"errors"
	"io/fs"
	"time"
)

// ErrNotExist is returned by Storage implementations for missing paths.
var ErrNotExist = fs.ErrNotExist

// IsNotExist reports whether err means "path does not exist".
func IsNotExist(err error) bool { return errors.Is(err, ErrNotExist) }

// Entry describes one file or directory in a group tree. Paths are
// group-absolute ("/a/b.json").
type Entry struct {
	Path  string
	Dir   bool
	Size  int64
	MTime time.Time
}

// Storage is the minimal remote/local surface a group needs. Implementations
// must be safe for concurrent readers; writes are serialised by the caller.
type Storage interface {
	List(ctx context.Context, dir string) ([]Entry, error)
	ReadFile(ctx context.Context, filePath string) ([]byte, error)
	WriteFile(ctx context.Context, filePath string, data []byte) error
	Remove(ctx context.Context, filePath string) error
	MkdirAll(ctx context.Context, dir string) error
	Stat(ctx context.Context, filePath string) (Entry, error)
}

// Walk visits every non-directory entry under root (excluding root itself).
func Walk(ctx context.Context, s Storage, root string) ([]Entry, error) {
	var out []Entry
	var rec func(string) error
	rec = func(dir string) error {
		entries, err := s.List(ctx, dir)
		if err != nil {
			if errors.Is(err, ErrNotExist) {
				return nil
			}
			return err
		}
		for _, e := range entries {
			if e.Dir {
				if err := rec(e.Path); err != nil {
					return err
				}
				continue
			}
			out = append(out, e)
		}
		return nil
	}
	if err := rec(root); err != nil {
		return nil, err
	}
	return out, nil
}

// ReadFileOrNil returns (nil, false) when the file does not exist.
func ReadFileOrNil(ctx context.Context, s Storage, p string) ([]byte, bool, error) {
	data, err := s.ReadFile(ctx, p)
	if err != nil {
		if errors.Is(err, ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return data, true, nil
}
