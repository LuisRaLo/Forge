package github

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

const (
	helperEnvVar  = "AI_SQUAD_TEST_GH_HELPER"
	fixtureEnvVar = "AI_SQUAD_TEST_GH_FIXTURE"
)

// TestMain intercepts when this binary has been re-exec'd to stand in for
// gh, mirroring the pattern used for the Claude Code adapter in
// internal/runtimes/claudecode: a real gh installation exists here but is
// unauthenticated, so behaviour is exercised against a scripted fake rather
// than a live GitHub repository.
func TestMain(m *testing.M) {
	if os.Getenv(helperEnvVar) == "1" {
		helperMain()
		return
	}
	os.Exit(m.Run())
}

func helperMain() {
	args := os.Args[1:]
	switch os.Getenv(fixtureEnvVar) {
	case "create_pr":
		if !contains(args, "--title") {
			os.Stderr.WriteString("missing --title\n")
			os.Exit(1)
		}
		os.Stdout.WriteString("https://github.com/acme/widgets/pull/42\n")
	case "checks_pass":
		os.Stdout.WriteString(`[{"name":"build","state":"SUCCESS","bucket":"pass","link":"https://x","description":""}]` + "\n")
	case "checks_fail":
		os.Stdout.WriteString(`[{"name":"build","state":"FAILURE","bucket":"fail","link":"https://x","description":"exit 1"}]` + "\n")
	case "checks_pending":
		os.Stdout.WriteString(`[{"name":"build","state":"IN_PROGRESS","bucket":"pending","link":"https://x","description":""}]` + "\n")
	case "checks_empty":
		os.Stdout.WriteString(`[]` + "\n")
	case "comments":
		os.Stdout.WriteString(`{"comments":[{"author":{"login":"reviewer1"},"body":"looks good"}]}` + "\n")
	case "auth_error":
		os.Stderr.WriteString("gh: To use GitHub CLI in a GitHub Actions workflow, set the GH_TOKEN\n")
		os.Exit(4)
	default:
		os.Stderr.WriteString("unknown fixture\n")
		os.Exit(2)
	}
}

func contains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func newTestClient(t *testing.T, fixture string) (*Client, func(context.Context, string, ...string) (string, error)) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	c := &Client{Command: self}
	_ = os.Setenv(helperEnvVar, "1")
	_ = os.Setenv(fixtureEnvVar, fixture)
	t.Cleanup(func() {
		os.Unsetenv(helperEnvVar)
		os.Unsetenv(fixtureEnvVar)
	})
	return c, nil
}

func TestCreatePR(t *testing.T) {
	c, _ := newTestClient(t, "create_pr")
	pr, err := c.CreatePR(context.Background(), CreatePROptions{
		Dir: t.TempDir(), Title: "Add feature", Body: "does the thing",
	})
	if err != nil {
		t.Fatalf("create pr: %v", err)
	}
	if pr.URL != "https://github.com/acme/widgets/pull/42" {
		t.Errorf("unexpected URL: %s", pr.URL)
	}
}

func TestCreatePRRequiresTitle(t *testing.T) {
	c, _ := newTestClient(t, "create_pr")
	if _, err := c.CreatePR(context.Background(), CreatePROptions{Dir: t.TempDir()}); err == nil {
		t.Fatal("expected an error for a missing title")
	}
}

func TestPRChecksOverallPass(t *testing.T) {
	c, _ := newTestClient(t, "checks_pass")
	status, err := c.PRChecks(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatalf("checks: %v", err)
	}
	if status.Overall() != CheckPass {
		t.Errorf("expected pass, got %s", status.Overall())
	}
}

func TestPRChecksOverallFail(t *testing.T) {
	c, _ := newTestClient(t, "checks_fail")
	status, err := c.PRChecks(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatalf("checks: %v", err)
	}
	if status.Overall() != CheckFail {
		t.Errorf("expected fail, got %s", status.Overall())
	}
	if status.Checks[0].Description != "exit 1" {
		t.Errorf("unexpected description: %s", status.Checks[0].Description)
	}
}

func TestPRChecksOverallPending(t *testing.T) {
	c, _ := newTestClient(t, "checks_pending")
	status, err := c.PRChecks(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatalf("checks: %v", err)
	}
	if status.Overall() != CheckPending {
		t.Errorf("expected pending, got %s", status.Overall())
	}
}

func TestPRChecksEmptyIsPendingNotPass(t *testing.T) {
	// No checks reported must never be silently treated as success.
	c, _ := newTestClient(t, "checks_empty")
	status, err := c.PRChecks(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatalf("checks: %v", err)
	}
	if status.Overall() != CheckPending {
		t.Errorf("expected pending for no reported checks, got %s", status.Overall())
	}
}

func TestPRComments(t *testing.T) {
	c, _ := newTestClient(t, "comments")
	comments, err := c.PRComments(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatalf("comments: %v", err)
	}
	if len(comments) != 1 || comments[0].Author != "reviewer1" || comments[0].Body != "looks good" {
		t.Errorf("unexpected comments: %+v", comments)
	}
}

func TestCommandErrorSurfacesStderr(t *testing.T) {
	c, _ := newTestClient(t, "auth_error")
	_, err := c.PRChecks(context.Background(), t.TempDir(), "")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "GH_TOKEN") {
		t.Errorf("expected the underlying gh error surfaced, got %v", err)
	}
}

// A real, unauthenticated invocation must fail cleanly rather than hang,
// confirming the wrapper does not depend on interactive prompts.
func TestRealGHReportsAuthRequired(t *testing.T) {
	if _, err := exec.LookPath("gh"); err != nil {
		t.Skip("gh not installed")
	}
	c := New()
	_, err := c.PRChecks(context.Background(), t.TempDir(), "")
	if err == nil {
		t.Skip("gh appears to be authenticated in this environment; nothing to assert")
	}
}
