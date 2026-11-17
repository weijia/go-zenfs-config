// Package gitee implements zenfsconfig.Storage on top of the Gitee Contents
// API. Gitee does not expose the Git Data write endpoints (POST blobs/trees/
// commits, PATCH refs all return 404), so every write is a single-file
// commit, exactly like zen-fs-gitee.
package gitee

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	zc "github.com/weijia/go-zenfs-config"
)

// Config configures a Gitee-backed group.
type Config struct {
	Owner   string
	Repo    string
	Branch  string // empty: resolved from the repo's default_branch
	Token   string
	BaseURL string // default https://gitee.com/api/v5
	// HTTPClient is optional; a 30s-timeout client is used by default.
	HTTPClient *http.Client
	// Message is the commit message prefix; defaults to "sync".
	Message string
}

// Backend is a Storage backed by a Gitee repository.
type Backend struct {
	cfg      Config
	base     string
	client   *http.Client
	mu       sync.Mutex
	shas     map[string]string
	branch   string
	initDone bool
	dirty    bool
	tree     []treeEntry
}

type treeEntry struct {
	Path string
	Type string // blob | tree
	Size int64
	Sha  string
}

// New validates cfg and returns a backend. The default branch is resolved
// lazily on first use so that an empty repository still works.
func New(cfg Config) (*Backend, error) {
	if cfg.Owner == "" || cfg.Repo == "" || cfg.Token == "" {
		return nil, errors.New("gitee: owner, repo and token are required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://gitee.com/api/v5"
	}
	cfg.BaseURL = strings.TrimSuffix(cfg.BaseURL, "/")
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.Message == "" {
		cfg.Message = "sync"
	}
	return &Backend{
		cfg:    cfg,
		base:   cfg.BaseURL + "/repos/" + url.PathEscape(cfg.Owner) + "/" + url.PathEscape(cfg.Repo),
		client: client,
		shas:   map[string]string{},
		branch: cfg.Branch,
	}, nil
}

func (b *Backend) ensureBranch(ctx context.Context) (string, error) {
	b.mu.Lock()
	if b.branch != "" {
		br := b.branch
		b.mu.Unlock()
		return br, nil
	}
	b.mu.Unlock()

	var repo struct {
		DefaultBranch string `json:"default_branch"`
	}
	// Private repositories answer 404 without a token, so the metadata request
	// must be authenticated too.
	if err := b.doJSON(ctx, http.MethodGet, b.auth(b.base), nil, &repo); err != nil {
		return "", err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.branch == "" {
		b.branch = repo.DefaultBranch
	}
	if b.branch == "" {
		b.branch = "master"
	}
	return b.branch, nil
}

func (b *Backend) doJSON(ctx context.Context, method, u string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("gitee: request failed: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == http.StatusNotFound {
		return zc.ErrNotExist
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("gitee: %s %s -> %d %s", method, redact(u), resp.StatusCode, trimBody(data))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("gitee: decode response: %w", err)
		}
	}
	return nil
}

func trimBody(data []byte) string {
	s := string(bytes.TrimSpace(data))
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	return s
}

// redact removes access_token from URLs that appear in error messages.
func redact(u string) string {
	if i := strings.Index(u, "access_token="); i >= 0 {
		rest := u[i+len("access_token="):]
		if j := strings.IndexByte(rest, '&'); j >= 0 {
			rest = rest[j:]
		} else {
			rest = ""
		}
		return u[:i] + "access_token=***" + rest
	}
	return u
}

func (b *Backend) auth(u string) string {
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + "access_token=" + url.QueryEscape(b.cfg.Token)
}

func (b *Backend) apiPath(p string) string {
	clean := strings.TrimPrefix(path.Clean("/"+p), "/")
	if clean == "" {
		return b.base + "/contents/"
	}
	segs := strings.Split(clean, "/")
	for i := range segs {
		segs[i] = url.PathEscape(segs[i])
	}
	return b.base + "/contents/" + strings.Join(segs, "/")
}

func (b *Backend) nowMillis() int64 { return time.Now().UnixMilli() }

// ensureInit creates the first commit in an empty repository by writing
// .keep with a newline, matching zen-fs-gitee's bootstrap.
func (b *Backend) ensureInit(ctx context.Context) error {
	b.mu.Lock()
	if b.initDone {
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	if _, err := b.ensureBranch(ctx); err != nil {
		return err
	}
	info, exists, err := b.fetchContent(ctx, zc.KeepPath)
	if err == nil && exists {
		b.mu.Lock()
		b.initDone = true
		if info.Sha != "" {
			b.shas[zc.KeepPath] = info.Sha
		}
		b.mu.Unlock()
		return nil
	}
	if err != nil && !errors.Is(err, zc.ErrNotExist) {
		return err
	}
	if err := b.putContent(ctx, zc.KeepPath, []byte("\n"), "chore: initialise group"); err != nil {
		return err
	}
	b.mu.Lock()
	b.initDone = true
	b.mu.Unlock()
	return nil
}

// List returns the immediate children of dir.
func (b *Backend) List(ctx context.Context, dir string) ([]zc.Entry, error) {
	if err := b.ensureInit(ctx); err != nil {
		return nil, err
	}
	entries, err := b.loadTree(ctx)
	if err != nil {
		return nil, err
	}
	prefix := strings.TrimSuffix(dir, "/")
	if prefix == "" {
		prefix = "/"
	}
	seen := map[string]bool{}
	out := make([]zc.Entry, 0)
	for _, e := range entries {
		full := "/" + e.Path
		if !strings.HasPrefix(full, prefix+"/") && prefix != "/" {
			continue
		}
		rest := strings.TrimPrefix(full, prefix)
		if prefix == "/" {
			rest = full
		}
		rest = strings.TrimPrefix(rest, "/")
		if rest == "" {
			continue
		}
		name := rest
		isDir := e.Type == "tree"
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			name = rest[:i]
			isDir = true
		}
		child := path.Join(prefix, name)
		if seen[child] {
			continue
		}
		seen[child] = true
		ent := zc.Entry{Path: child, Dir: isDir}
		if !isDir {
			ent.Size = e.Size
		}
		out = append(out, ent)
	}
	return out, nil
}

func (b *Backend) loadTree(ctx context.Context) ([]treeEntry, error) {
	b.mu.Lock()
	if !b.dirty && b.tree != nil {
		t := b.tree
		b.mu.Unlock()
		return t, nil
	}
	b.mu.Unlock()

	br, err := b.ensureBranch(ctx)
	if err != nil {
		return nil, err
	}
	u := b.auth(b.base + "/git/trees/" + url.PathEscape(br) + "?recursive=1")
	var res struct {
		Tree []treeEntry `json:"tree"`
	}
	if err := b.doJSON(ctx, http.MethodGet, u, nil, &res); err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.tree = res.Tree
	b.dirty = false
	for _, e := range res.Tree {
		if e.Type == "blob" {
			b.shas["/"+e.Path] = e.Sha
		}
	}
	b.mu.Unlock()
	return res.Tree, nil
}

type contentInfo struct {
	Content     string `json:"content"`
	Encoding    string `json:"encoding"`
	Sha         string `json:"sha"`
	DownloadURL string `json:"download_url"`
}

// fetchContent reads one path through the Contents API.
//
// Gitee returns 200 with a JSON array (directory listing) for a path that
// does not exist, instead of 404, so existence must be decided by the body
// shape: object = file, array = directory or missing.
func (b *Backend) fetchContent(ctx context.Context, p string) (contentInfo, bool, error) {
	br, err := b.ensureBranch(ctx)
	if err != nil {
		return contentInfo{}, false, err
	}
	u := b.auth(b.apiPath(p) + "?ref=" + url.QueryEscape(br))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return contentInfo{}, false, err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return contentInfo{}, false, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == http.StatusNotFound {
		return contentInfo{}, false, zc.ErrNotExist
	}
	if resp.StatusCode >= 400 {
		return contentInfo{}, false, fmt.Errorf("gitee: GET %s -> %d %s", redact(u), resp.StatusCode, trimBody(data))
	}
	trim := bytes.TrimSpace(data)
	if len(trim) == 0 || trim[0] == '[' {
		return contentInfo{}, false, nil
	}
	var info contentInfo
	if err := json.Unmarshal(trim, &info); err != nil {
		return contentInfo{}, false, fmt.Errorf("gitee: decode contents of %s: %w", p, err)
	}
	return info, true, nil
}

func (b *Backend) ReadFile(ctx context.Context, p string) ([]byte, error) {
	if err := b.ensureInit(ctx); err != nil {
		return nil, err
	}
	res, exists, err := b.fetchContent(ctx, p)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, zc.ErrNotExist
	}
	if res.Encoding == "base64" && res.Content != "" {
		data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(res.Content, "\n", ""))
		if err != nil {
			return nil, fmt.Errorf("gitee: decode %s: %w", p, err)
		}
		b.cacheSha(p, res.Sha)
		return data, nil
	}
	// Large files: the Contents API omits the body; fetch the raw blob.
	br, err := b.ensureBranch(ctx)
	if err != nil {
		return nil, err
	}
	rawU := b.auth(b.base + "/raw/" + escapeSegments(strings.TrimPrefix(p, "/")) + "?ref=" + url.QueryEscape(br))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawU, nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, zc.ErrNotExist
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("gitee: raw %s -> %d", p, resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	b.cacheSha(p, res.Sha)
	return data, nil
}

func escapeSegments(p string) string {
	segs := strings.Split(p, "/")
	for i := range segs {
		segs[i] = url.PathEscape(segs[i])
	}
	return strings.Join(segs, "/")
}

func (b *Backend) cacheSha(p, sha string) {
	if sha == "" {
		return
	}
	b.mu.Lock()
	b.shas["/"+strings.TrimPrefix(p, "/")] = sha
	b.mu.Unlock()
}

func (b *Backend) getSha(ctx context.Context, p string) (string, bool, error) {
	key := "/" + strings.TrimPrefix(p, "/")
	b.mu.Lock()
	sha, ok := b.shas[key]
	b.mu.Unlock()
	if ok {
		return sha, true, nil
	}
	info, exists, err := b.fetchContent(ctx, p)
	if err != nil {
		if errors.Is(err, zc.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	if !exists || info.Sha == "" {
		return "", false, nil
	}
	b.cacheSha(p, info.Sha)
	return info.Sha, true, nil
}

func (b *Backend) WriteFile(ctx context.Context, p string, data []byte) error {
	if err := b.ensureInit(ctx); err != nil {
		return err
	}
	if err := b.putContent(ctx, p, data, b.cfg.Message+": "+strings.TrimPrefix(p, "/")); err != nil {
		return err
	}
	if strings.HasSuffix(p, ".mtime") {
		return nil
	}
	return b.writeMtime(ctx, p)
}

func (b *Backend) putContent(ctx context.Context, p string, data []byte, message string) error {
	br, err := b.ensureBranch(ctx)
	if err != nil {
		return err
	}
	form := url.Values{}
	form.Set("access_token", b.cfg.Token)
	form.Set("content", base64.StdEncoding.EncodeToString(data))
	form.Set("message", message)
	form.Set("branch", br)
	sha, exists, err := b.getSha(ctx, p)
	if err != nil {
		return err
	}
	method := http.MethodPost
	u := b.auth(b.apiPath(p))
	if exists {
		method = http.MethodPut
		form.Set("sha", sha)
	}
	var res struct {
		Content struct {
			Sha string `json:"sha"`
		} `json:"content"`
	}
	if err := b.doJSON(ctx, method, u, strings.NewReader(form.Encode()), &res); err != nil {
		return err
	}
	b.cacheSha(p, res.Content.Sha)
	b.mu.Lock()
	b.dirty = true
	b.mu.Unlock()
	return nil
}

// writeMtime stores the plain-decimal mtime sidecar used by zen-fs-gitee.
// It is idempotent: an equal value produces no commit.
func (b *Backend) writeMtime(ctx context.Context, p string) error {
	mp := p + ".mtime"
	value := []byte(strconv.FormatInt(b.nowMillis(), 10))
	if cur, ok, err := zc.ReadFileOrNil(ctx, b, mp); err == nil && ok && bytes.Equal(cur, value) {
		return nil
	}
	return b.putContent(ctx, mp, value, b.cfg.Message+": mtime "+strings.TrimPrefix(p, "/"))
}

func (b *Backend) Remove(ctx context.Context, p string) error {
	_ = b.removeOne(ctx, p)
	_ = b.removeOne(ctx, p+".mtime")
	return nil
}

func (b *Backend) removeOne(ctx context.Context, p string) error {
	br, err := b.ensureBranch(ctx)
	if err != nil {
		return err
	}
	sha, exists, err := b.getSha(ctx, p)
	if err != nil {
		return err
	}
	if !exists {
		return zc.ErrNotExist
	}
	form := url.Values{}
	form.Set("access_token", b.cfg.Token)
	form.Set("sha", sha)
	form.Set("message", b.cfg.Message+": delete "+strings.TrimPrefix(p, "/"))
	form.Set("branch", br)
	if err := b.doJSON(ctx, http.MethodDelete, b.auth(b.apiPath(p)), strings.NewReader(form.Encode()), nil); err != nil {
		return err
	}
	b.mu.Lock()
	delete(b.shas, "/"+strings.TrimPrefix(p, "/"))
	b.dirty = true
	b.mu.Unlock()
	return nil
}

// MkdirAll is a no-op: Git has no directories, empty dirs cannot be stored.
func (b *Backend) MkdirAll(context.Context, string) error { return nil }

func (b *Backend) Stat(ctx context.Context, p string) (zc.Entry, error) {
	if err := b.ensureInit(ctx); err != nil {
		return zc.Entry{}, err
	}
	entries, err := b.loadTree(ctx)
	if err != nil {
		return zc.Entry{}, err
	}
	key := strings.TrimPrefix(p, "/")
	for _, e := range entries {
		if e.Path != key {
			continue
		}
		ent := zc.Entry{Path: p, Dir: e.Type == "tree", Size: e.Size}
		if e.Type == "blob" {
			if mt, err := b.mtimeOf(ctx, p); err == nil {
				ent.MTime = time.UnixMilli(mt)
			}
		}
		return ent, nil
	}
	return zc.Entry{}, zc.ErrNotExist
}

func (b *Backend) mtimeOf(ctx context.Context, p string) (int64, error) {
	data, ok, err := zc.ReadFileOrNil(ctx, b, p+".mtime")
	if err != nil || !ok {
		return 0, err
	}
	ms, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, nil
	}
	return ms, nil
}

// Branch returns the resolved branch name (after first use).
func (b *Backend) Branch() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.branch
}
