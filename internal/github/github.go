// Package github wraps the GitHub CLI (`gh`) for the operations the
// orchestrator's V1 GitHub integration needs: opening a pull request,
// checking its CI status, and reading its comments. It never merges and
// never deploys — those stay outside this package's scope entirely, so
// there is no method that could be misused to automate either.
//
// Flag shapes below were verified against the installed CLI (`gh` 2.100.0 —
// `gh pr create --help`, `gh pr checks --help`, `gh pr view --help`), not
// guessed. `gh` was not authenticated on the machine this was built on
// (`gh auth status` reported not logged in), so unlike the Claude Code
// adapter, no live call against a real GitHub repository was made — the
// flag contracts are confirmed, the end-to-end behaviour against a real PR
// is not. See docs/architecture.md.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Client runs gh commands. The zero value is ready to use and resolves "gh"
// on PATH.
type Client struct {
	// Command overrides the gh executable, mainly for tests.
	Command string
}

// New returns a Client using "gh" on PATH.
func New() *Client { return &Client{} }

func (c *Client) bin() string {
	if c.Command == "" {
		return "gh"
	}
	return c.Command
}

// CommandError reports a failed gh invocation with its stderr attached.
type CommandError struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *CommandError) Error() string {
	msg := "gh " + strings.Join(e.Args, " ")
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

// PullRequest describes a pull request the orchestrator opened or is
// inspecting.
type PullRequest struct {
	URL string
}

// CreatePROptions configures pull request creation.
type CreatePROptions struct {
	// Dir is the git working directory (a task's workspace) gh runs in.
	Dir   string
	Title string
	Body  string
	Base  string // target branch; empty uses the repository default
	Head  string // source branch; empty uses the current branch
	Draft bool
}

// CreatePR opens a pull request via `gh pr create`, which prints the new
// PR's URL on success — verified directly in its --help text ("Upon
// success, the URL of the created pull request will be printed").
func (c *Client) CreatePR(ctx context.Context, opts CreatePROptions) (*PullRequest, error) {
	if opts.Dir == "" {
		return nil, fmt.Errorf("dir must be set")
	}
	if opts.Title == "" {
		return nil, fmt.Errorf("title must be set")
	}
	args := []string{"pr", "create", "--title", opts.Title, "--body", opts.Body}
	if opts.Base != "" {
		args = append(args, "--base", opts.Base)
	}
	if opts.Head != "" {
		args = append(args, "--head", opts.Head)
	}
	if opts.Draft {
		args = append(args, "--draft")
	}
	url, err := c.run(ctx, opts.Dir, args...)
	if err != nil {
		return nil, fmt.Errorf("create pull request: %w", err)
	}
	return &PullRequest{URL: url}, nil
}

// CheckBucket is gh's own coarse categorization of a check's state, per
// `gh pr checks --help`: "pass, fail, pending, skipping, or cancel".
type CheckBucket string

const (
	CheckPass     CheckBucket = "pass"
	CheckFail     CheckBucket = "fail"
	CheckPending  CheckBucket = "pending"
	CheckSkipping CheckBucket = "skipping"
	CheckCancel   CheckBucket = "cancel"
)

// Check is one CI check's status.
type Check struct {
	Name        string      `json:"name"`
	State       string      `json:"state"`
	Bucket      CheckBucket `json:"bucket"`
	Link        string      `json:"link"`
	Description string      `json:"description"`
}

// Status summarises every check on a pull request.
type Status struct {
	Checks []Check
}

// Overall reports the aggregate CI status: "fail" if any check failed,
// else "pending" if any is still running, else "pass". An empty check list
// (no CI configured, or none reported yet) is "pending" — the orchestrator
// must never treat "no information" as "passed."
func (s Status) Overall() CheckBucket {
	if len(s.Checks) == 0 {
		return CheckPending
	}
	sawPending := false
	for _, c := range s.Checks {
		switch c.Bucket {
		case CheckFail, CheckCancel:
			return CheckFail
		case CheckPending:
			sawPending = true
		}
	}
	if sawPending {
		return CheckPending
	}
	return CheckPass
}

// PRChecks fetches CI status for the pull request associated with dir's
// current branch (or ref, if given). ref may be empty, a PR number, or a
// branch name, matching `gh pr checks [<number> | <url> | <branch>]`.
func (c *Client) PRChecks(ctx context.Context, dir, ref string) (*Status, error) {
	args := []string{"pr", "checks"}
	if ref != "" {
		args = append(args, ref)
	}
	args = append(args, "--json", "name,state,bucket,link,description")

	out, err := c.run(ctx, dir, args...)
	if err != nil {
		// Exit code 8 ("checks pending", per --help) is a normal outcome,
		// not a failure — gh still writes JSON in that case, so callers
		// only reach this branch for a genuine command failure (auth,
		// network, no such PR).
		return nil, fmt.Errorf("fetch PR checks: %w", err)
	}
	var checks []Check
	if out != "" {
		if err := json.Unmarshal([]byte(out), &checks); err != nil {
			return nil, fmt.Errorf("parse PR checks: %w", err)
		}
	}
	return &Status{Checks: checks}, nil
}

// Comment is one comment on a pull request.
type Comment struct {
	Author string `json:"author"`
	Body   string `json:"body"`
}

type prCommentsResponse struct {
	Comments []struct {
		Author struct {
			Login string `json:"login"`
		} `json:"author"`
		Body string `json:"body"`
	} `json:"comments"`
}

// PRComments fetches a pull request's comments via `gh pr view --json
// comments`.
func (c *Client) PRComments(ctx context.Context, dir, ref string) ([]Comment, error) {
	args := []string{"pr", "view"}
	if ref != "" {
		args = append(args, ref)
	}
	args = append(args, "--json", "comments")

	out, err := c.run(ctx, dir, args...)
	if err != nil {
		return nil, fmt.Errorf("fetch PR comments: %w", err)
	}
	var resp prCommentsResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return nil, fmt.Errorf("parse PR comments: %w", err)
	}
	out2 := make([]Comment, len(resp.Comments))
	for i, cm := range resp.Comments {
		out2[i] = Comment{Author: cm.Author.Login, Body: cm.Body}
	}
	return out2, nil
}
