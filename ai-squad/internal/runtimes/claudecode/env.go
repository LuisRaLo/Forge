package claudecode

import (
	"os"
	"strings"
)

// passthroughEnv is an allowlist, deliberately: the orchestrator process may
// hold credentials for OTHER providers (DEEPSEEK_API_KEY, OPENAI_API_KEY, a
// GitHub token, ...) in its own environment. Passing the full environment
// through to Claude Code would let an agent's Bash tool read every one of
// them with `env`. Claude Code itself authenticates via OAuth/keychain state
// under $HOME, not via an env var, so in the common case it needs none of
// this at all.
var passthroughEnv = map[string]bool{
	"PATH": true, "HOME": true, "TERM": true, "LANG": true, "LC_ALL": true,
	"USER": true, "SHELL": true, "TMPDIR": true,
}

// childEnv builds the minimal environment for the Claude Code process, with
// extra appended verbatim. extra exists for tests; production callers pass
// none.
func childEnv(extra ...string) []string {
	out := make([]string, 0, len(passthroughEnv)+len(extra))
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if ok && passthroughEnv[name] {
			out = append(out, kv)
		}
	}
	return append(out, extra...)
}
