// Package folder implements zenfsconfig.Storage on a local directory.
package folder

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	zc "github.com/weijia/go-zenfs-config"
)

// Backend stores a group under a local directory.
type Backend struct {
	root string
}

// New opens (and creates) root.
func New(root string) (*Backend, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	return &Backend{root: abs}, nil
}

// Root returns the absolute directory backing this group.
func (b *Backend) Root() string { return b.root }

func (b *Backend) path(p string) (string, error) {
	clean := strings.TrimPrefix(filepath.ToSlash(p), "/")
	if clean == "" || clean == "." {
		return b.root, nil
	}
	if strings.Contains(clean, "..") {
		return "", errors.New("folder: path escapes root: " + p)
	}
	return filepath.Join(b.root, filepath.FromSlash(clean)), nil
}

func (b *Backend) List(_ context.Context, dir string) ([]zc.Entry, error) {
	full, err := b.path(dir)
	if err != nil {
		return nil, err
	}
	items, err := os.ReadDir(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, zc.ErrNotExist
		}
		return nil, err
	}
	out := make([]zc.Entry, 0, len(items))
	base := strings.TrimSuffix(filepath.ToSlash(dir), "/")
	for _, it := range items {
		info, err := it.Info()
		if err != nil {
			continue
		}
		out = append(out, zc.Entry{
			Path:  base + "/" + it.Name(),
			Dir:   it.IsDir(),
			Size:  info.Size(),
			MTime: info.ModTime(),
		})
	}
	return out, nil
}

func (b *Backend) ReadFile(_ context.Context, p string) ([]byte, error) {
	full, err := b.path(p)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, zc.ErrNotExist
		}
		return nil, err
	}
	return data, nil
}

func (b *Backend) WriteFile(_ context.Context, p string, data []byte) error {
	full, err := b.path(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, data, 0o644)
}

func (b *Backend) Remove(_ context.Context, p string) error {
	full, err := b.path(p)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return zc.ErrNotExist
		}
		return err
	}
	return nil
}

func (b *Backend) MkdirAll(_ context.Context, dir string) error {
	full, err := b.path(dir)
	if err != nil {
		return err
	}
	return os.MkdirAll(full, 0o755)
}

func (b *Backend) Stat(_ context.Context, p string) (zc.Entry, error) {
	full, err := b.path(p)
	if err != nil {
		return zc.Entry{}, err
	}
	info, err := os.Stat(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return zc.Entry{}, zc.ErrNotExist
		}
		return zc.Entry{}, err
	}
	return zc.Entry{Path: p, Dir: info.IsDir(), Size: info.Size(), MTime: info.ModTime()}, nil
}
