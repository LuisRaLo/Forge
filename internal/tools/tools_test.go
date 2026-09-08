package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/LuisRaLo/ai-squad/internal/core"
)

func TestSetReflectsPermissions(t *testing.T) {
	t.Parallel()

	names := func(perms core.Permissions) []string {
		var out []string
		for _, tool := range Set(perms) {
			out = append(out, tool.Spec().Name)
		}
		return out
	}

	if got := names(core.Permissions{Filesystem: core.FSNone}); len(got) != 0 {
		t.Errorf("no filesystem, no shell should offer no tools, got %v", got)
	}
	if got := names(core.Permissions{Filesystem: core.FSRead}); len(got) != 1 || got[0] != "read_file" {
		t.Errorf("read-only should offer just read_file, got %v", got)
	}
	if got := names(core.Permissions{Filesystem: core.FSWorkspace}); len(got) != 2 {
		t.Errorf("workspace access should offer read and write, got %v", got)
	}
	if got := names(core.Permissions{Filesystem: core.FSWorkspace, Shell: core.ShellPolicy{Enabled: true}}); len(got) != 3 {
		t.Errorf("workspace+shell should offer 3 tools, got %v", got)
	}
}

func TestReadWriteRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.Background()

	w := writeTool{}
	res, err := w.Invoke(ctx, dir, json.RawMessage(`{"path":"sub/a.txt","content":"hello"}`))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected write error: %s", res.Content)
	}

	r := readTool{}
	res, err = r.Invoke(ctx, dir, json.RawMessage(`{"path":"sub/a.txt"}`))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if res.IsError || res.Content != "hello" {
		t.Fatalf("unexpected read result: %+v", res)
	}
}

func TestReadWriteRejectPathEscape(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.Background()

	// Seed a file outside the workspace that a naive join could reach.
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("do not read me"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	r := readTool{}
	rel, err := filepath.Rel(dir, secret)
	if err != nil {
		t.Fatalf("rel: %v", err)
	}
	res, err := r.Invoke(ctx, dir, json.RawMessage(`{"path":"`+filepath.ToSlash(rel)+`"}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected a path-escape attempt to be rejected")
	}

	w := writeTool{}
	res, err = w.Invoke(ctx, dir, json.RawMessage(`{"path":"../../etc/passwd","content":"pwned"}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected a write path-escape attempt to be rejected")
	}
}

func TestReadMissingFile(t *testing.T) {
	t.Parallel()
	r := readTool{}
	res, err := r.Invoke(context.Background(), t.TempDir(), json.RawMessage(`{"path":"ghost.txt"}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for a missing file")
	}
}

func TestBashAllowsAndDenies(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.Background()

	allowed := bashTool{policy: core.ShellPolicy{Enabled: true, Commands: []string{"echo"}}}
	res, err := allowed.Invoke(ctx, dir, json.RawMessage(`{"command":"echo hi"}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected echo to succeed: %s", res.Content)
	}

	denied := bashTool{policy: core.ShellPolicy{Enabled: true, Commands: []string{"echo"}}}
	res, err = denied.Invoke(ctx, dir, json.RawMessage(`{"command":"curl http://evil"}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected a disallowed command to be rejected")
	}

	fullyDisabled := bashTool{policy: core.ShellPolicy{Enabled: false}}
	res, err = fullyDisabled.Invoke(ctx, dir, json.RawMessage(`{"command":"echo hi"}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !res.IsError {
		t.Fatal("a disabled shell must reject every command")
	}
}

func TestBashRunsInWorkspaceDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tool := bashTool{policy: core.ShellPolicy{Enabled: true, Commands: []string{"ls"}}}
	res, err := tool.Invoke(context.Background(), dir, json.RawMessage(`{"command":"ls"}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !contains(res.Content, "marker.txt") {
		t.Errorf("expected ls output to list marker.txt, got %q", res.Content)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
