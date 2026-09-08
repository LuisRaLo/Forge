package claudecode

import "regexp"

// secretPatterns catch common secret shapes so they never reach a log, an
// error message, or the audit trail. Claude Code authenticates via its own
// OAuth/keychain state, not via a secret this adapter handles directly, so
// this is defense in depth rather than a known leak path: stderr or an error
// string could still echo something sensitive from the workspace or from a
// tool a permitted shell agent ran.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{10,}`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._-]{10,}`),
	regexp.MustCompile(`(?i)(api[_-]?key|token|secret|password)["']?\s*[:=]\s*["']?[A-Za-z0-9._-]{8,}`),
}

// redact scrubs recognisable secret shapes from s.
func redact(s string) string {
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, "[REDACTED]")
	}
	return s
}
