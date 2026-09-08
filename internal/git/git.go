// Package git wraps the git CLI for the operations the orchestrator needs.
// It is deliberately thin: worktree management for Phase 3, extended with
// commit/push/PR-adjacent operations in Phase 7 rather than speculatively
// built out now.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Client runs git commands. The zero value is ready to use and resolves
// "git" on PATH.
type Client struct {
	// Command overrides the git executable, mainly for tests.
	Command string
}

// New returns a Client using "git" on PATH.
func New() *Client { return &Client{} }

func (c *Client) bin() string {
	if c.Command == "" {
		return "git"
	}
	return c.Command
}

// CommandError reports a failed git invocation with its stderr attached, so
// callers see git's own explanation rather than just an exit code.
type CommandError struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *CommandError) Error() string {
	msg := strings.Join(append([]string{"git"}, e.Args...), " ")
	if e.Stderr != "" {
		return fmt.Sprintf("%s: %s", msg, e.Stderr)
	}
	return fmt.Sprintf("%s: %s", msg, e.Err)
}

func (e *CommandError) Unwrap() error { return e.Err }

func (c *Client) run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, c.bin(), args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", &CommandError{Args: args, Stderr: strings.TrimSpace(stderr.String()), Err: err}
	}
	return strings.TrimSpace(stdout.String()), nil
}

// IsRepository reports whether dir is inside a git working tree.
func (c *Client) IsRepository(ctx context.Context, dir string) bool {
	_, err := c.run(ctx, dir, "rev-parse", "--git-dir")
	return err == nil
}

// BranchExists reports whether branch exists locally in the repository at
// dir.
func (c *Client) BranchExists(ctx context.Context, dir, branch string) (bool, error) {
	_, err := c.run(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err == nil {
		return true, nil
	}
	var ce *CommandError
	if errors.As(err, &ce) {
		return false, nil
	}
	return false, err
}

// WorktreeAdd creates a new worktree at path checked out to branch. If branch
// already exists locally it is checked out as-is; otherwise it is created
// from base (an empty base means the repository's current HEAD).
func (c *Client) WorktreeAdd(ctx context.Context, repoDir, path, branch, base string) error {
	exists, err := c.BranchExists(ctx, repoDir, branch)
	if err != nil {
		return fmt.Errorf("check branch %s: %w", branch, err)
	}

	args := []string{"worktree", "add"}
	if exists {
		args = append(args, path, branch)
	} else {
		args = append(args, "-b", branch, path)
		if base != "" {
			args = append(args, base)
		}
	}
	_, err = c.run(ctx, repoDir, args...)
	return err
}

// WorktreeRemove removes a worktree. force discards uncommitted changes;
// without it, git refuses to remove a dirty worktree.
func (c *Client) WorktreeRemove(ctx context.Context, repoDir, path string, force bool) error {
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, path)
	_, err := c.run(ctx, repoDir, args...)
	return err
}

// WorktreePrune removes stale worktree administrative data left behind when a
// worktree directory was deleted outside of WorktreeRemove.
func (c *Client) WorktreePrune(ctx context.Context, repoDir string) error {
	_, err := c.run(ctx, repoDir, "worktree", "prune")
	return err
}

// WorktreeEntry is one entry from `git worktree list`.
type WorktreeEntry struct {
	Path     string
	Branch   string
	Head     string
	Detached bool
	Locked   bool
}

// WorktreeList lists every worktree registered against the repository at
// repoDir, including the main one.
func (c *Client) WorktreeList(ctx context.Context, repoDir string) ([]WorktreeEntry, error) {
	out, err := c.run(ctx, repoDir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseWorktreeList(out), nil
}

func parseWorktreeList(out string) []WorktreeEntry {
	var entries []WorktreeEntry
	var cur WorktreeEntry

	flush := func() {
		if cur.Path != "" {
			entries = append(entries, cur)
		}
		cur = WorktreeEntry{}
	}

	for _, line := range strings.Split(out, "\n") {
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "worktree "):
			cur.Path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "HEAD "):
			cur.Head = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch "):
			cur.Branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		case line == "detached":
			cur.Detached = true
		case line == "locked" || strings.HasPrefix(line, "locked "):
			cur.Locked = true
		}
	}
	flush()
	return entries
}

// Commit stages every change in dir and commits it. An empty diff (nothing
// to commit) is not an error: it is reported via the bool return so callers
// can treat "nothing changed" as success rather than parsing git's message.
func (c *Client) Commit(ctx context.Context, dir, message string) (committed bool, err error) {
	if _, err := c.run(ctx, dir, "add", "-A"); err != nil {
		return false, fmt.Errorf("stage changes: %w", err)
	}
	if _, err := c.run(ctx, dir, "diff", "--cached", "--quiet"); err == nil {
		return false, nil // nothing staged
	}
	if _, err := c.run(ctx, dir, "commit", "-m", message); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}

// Push pushes branch to remote, creating the upstream tracking ref if it
// does not exist yet (-u), which is what lets `gh pr create` find the
// branch immediately afterward without an extra round trip.
func (c *Client) Push(ctx context.Context, dir, remote, branch string) error {
	_, err := c.run(ctx, dir, "push", "-u", remote, branch)
	return err
}

// CurrentBranch returns the branch checked out at dir.
func (c *Client) CurrentBranch(ctx context.Context, dir string) (string, error) {
	return c.run(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
}

// HeadCommit returns the commit hash HEAD points to at dir.
func (c *Client) HeadCommit(ctx context.Context, dir string) (string, error) {
	return c.run(ctx, dir, "rev-parse", "HEAD")
}

// IsDirty reports whether dir has uncommitted changes (staged, unstaged, or
// untracked).
func (c *Client) IsDirty(ctx context.Context, dir string) (bool, error) {
	out, err := c.run(ctx, dir, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// CommitsSince counts commits reachable from HEAD but not from baseCommit —
// i.e. how far dir's current branch has moved beyond a known starting point.
func (c *Client) CommitsSince(ctx context.Context, dir, baseCommit string) (int, error) {
	out, err := c.run(ctx, dir, "rev-list", "--count", baseCommit+"..HEAD")
	if err != nil {
		return 0, err
	}
	n, convErr := strconv.Atoi(out)
	if convErr != nil {
		return 0, fmt.Errorf("parse commit count %q: %w", out, convErr)
	}
	return n, nil
}

// MergeBase returns the commit where refA and refB diverged. Worktrees of
// the same repository share one object database, so this can be computed
// from any worktree's directory regardless of which one refA/refB actually
// belong to — including a branch created moments earlier in a different
// worktree via `git worktree add -b`.
func (c *Client) MergeBase(ctx context.Context, dir, refA, refB string) (string, error) {
	return c.run(ctx, dir, "merge-base", refA, refB)
}
