package gitee

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	zc "github.com/weijia/go-zenfs-config"
)

// TestLiveRoundTrip exercises the real Gitee Contents API on an isolated
// branch. It is skipped unless GITEE_TOKEN is set.
func TestLiveRoundTrip(t *testing.T) {
	token := os.Getenv("GITEE_TOKEN")
	if token == "" {
		t.Skip("GITEE_TOKEN not set")
	}
	const owner, repo, branch = "weijia432", "openlist-data", "go-sync-test"
	ctx := context.Background()

	if err := ensureBranch(ctx, token, owner, repo, branch); err != nil {
		t.Fatalf("ensure branch: %v", err)
	}
	backend, err := New(Config{Owner: owner, Repo: repo, Branch: branch, Token: token, Message: "go-test"})
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	rep, err := zc.Open(ctx, backend, "openlist")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	rep.Author = "go-test/node-1"

	const key = "settings/site_title"
	value := map[string]any{"title": "OpenList Go Round Trip", "locale": "zh"}

	changed, err := rep.SetConfig(ctx, key, value)
	if err != nil {
		t.Fatalf("set config: %v", err)
	}
	if !changed {
		t.Fatalf("first write should report changed=true")
	}
	raw, err := rep.ReadConfig(ctx, key)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	want, err := zc.MarshalConfigValue(value)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(want) {
		t.Fatalf("read-back differs\n got: %s\nwant: %s", raw, want)
	}

	vmeta, err := rep.ReadVersion(ctx, rep.ConfigPath(key))
	if err != nil || vmeta == nil {
		t.Fatalf("version sidecar: %v %v", vmeta, err)
	}
	if vmeta.Version != 1 || !strings.HasPrefix(vmeta.Hash, "sha256:") {
		t.Fatalf("unexpected version meta: %+v", vmeta)
	}

	// Same value again: no write, no version bump.
	changed, err = rep.SetConfig(ctx, key, value)
	if err != nil || changed {
		t.Fatalf("idempotent write: changed=%v err=%v", changed, err)
	}

	// New value: version must become 2.
	if _, err := rep.SetConfig(ctx, key, map[string]any{"title": "second write"}); err != nil {
		t.Fatalf("second set: %v", err)
	}
	vmeta2, err := rep.ReadVersion(ctx, rep.ConfigPath(key))
	if err != nil || vmeta2 == nil || vmeta2.Version != 2 {
		t.Fatalf("version should be 2, got %+v err=%v", vmeta2, err)
	}

	// Delete: tombstone must exist and the file must be gone.
	if err := rep.DeleteConfig(ctx, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := rep.ReadConfig(ctx, key); err == nil {
		t.Fatalf("file still readable after delete")
	}
	ts, err := zc.TombstoneAt(ctx, backend, rep.ConfigPath(key))
	if err != nil || ts == nil {
		t.Fatalf("tombstone missing: %+v %v", ts, err)
	}
	if ts.Path != rep.ConfigPath(key) {
		t.Fatalf("tombstone path = %q", ts.Path)
	}

	// Cleanup the test file's tombstone so the branch stays tidy.
	_ = backend.Remove(ctx, zc.TombstonePath(rep.ConfigPath(key)))
}

func ensureBranch(ctx context.Context, token, owner, repo, branch string) error {
	base := "https://gitee.com/api/v5/repos/" + owner + "/" + repo + "/branches"
	form := url.Values{}
	form.Set("access_token", token)
	form.Set("refs", "refs/heads/master")
	form.Set("branch_name", branch)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		// Already exists is fine.
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		if strings.Contains(e.Message, "already exists") || strings.Contains(e.Message, "已存在") {
			return nil
		}
		return &branchError{code: resp.StatusCode, body: string(data)}
	}
	return nil
}

type branchError struct {
	code int
	body string
}

func (e *branchError) Error() string {
	return "create branch failed: " + http.StatusText(e.code) + " " + e.body
}
