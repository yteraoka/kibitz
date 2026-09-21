package telemetry

import "regexp"

// Placeholder replaces any secret found in a log record.
const Placeholder = "[REDACTED]"

// redactions are applied to every string that reaches the logger. They are a
// backstop, not the primary defence: credentials are held in config.Secret,
// which never renders itself. This catches the cases that slip through, such
// as a token echoed inside an error string from an external library.
var redactions = []struct {
	re   *regexp.Regexp
	with string
}{
	// GitHub: ghp_/gho_/ghu_/ghs_/ghr_ tokens and fine-grained PATs.
	{regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`), Placeholder},
	{regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`), Placeholder},
	// GitLab personal, project and group access tokens.
	{regexp.MustCompile(`glpat-[A-Za-z0-9_\-]{16,}`), Placeholder},
	// Anthropic API keys, in case a model provider is configured with one.
	{regexp.MustCompile(`sk-ant-[A-Za-z0-9\-_]{16,}`), Placeholder},
	// Google OAuth access tokens (ADC) as they appear in transport errors.
	{regexp.MustCompile(`ya29\.[A-Za-z0-9_\-]{20,}`), Placeholder},
	// Private keys of any flavour, including the GitHub App key.
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`), Placeholder},
	{regexp.MustCompile(`(?i)"private_key"\s*:\s*"[^"]*"`), `"private_key":"` + Placeholder + `"`},
	// Authorization headers.
	{regexp.MustCompile(`(?i)(bearer|basic|token)\s+[A-Za-z0-9\-._~+/]{8,}={0,2}`), "$1 " + Placeholder},
	// Credentials embedded in a clone URL: https://user:token@host/...
	{regexp.MustCompile(`(?i)(https?://)[^/\s:@]+:[^/\s@]+@`), "${1}" + Placeholder + "@"},
}

// Redact removes anything that looks like a credential from s.
func Redact(s string) string {
	if s == "" {
		return s
	}
	for _, r := range redactions {
		if r.re.MatchString(s) {
			s = r.re.ReplaceAllString(s, r.with)
		}
	}
	return s
}
