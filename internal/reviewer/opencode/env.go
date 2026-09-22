package opencode

import (
	"os"
	"sort"
	"strings"
)

// baseEnv is what the agent's process is given from the worker's own
// environment, and the only thing it is given.
//
// The worker's environment holds the webhook secrets, the GitHub App's private
// key and the GitLab token. None of that is the agent's business, and an MCP
// server — a binary kibitz did not write, started by opencode, inheriting
// opencode's environment — is the last place any of it should end up. So the
// child gets a list rather than a copy: the things any program needs to run,
// plus whatever the provider and the enabled MCP servers were told to use.
//
// A deployment that needs one more variable names it in
// KIBITZ_AGENT_ENV_PASSTHROUGH rather than going back to inheriting all of
// them.
var baseEnv = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TZ",
	"LANG", "LC_ALL", "TERM",
	"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "CURL_CA_BUNDLE", "NODE_EXTRA_CA_CERTS",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy",

	// Application Default Credentials. On Cloud Run the token comes from the
	// metadata server and none of these are set, but a workstation and the
	// emulator both rely on them.
	"GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_QUOTA_PROJECT",
	"GOOGLE_CLOUD_REGION", "GOOGLE_CLOUD_LOCATION", "CLOUD_ML_REGION",
	"GCE_METADATA_HOST", "GCE_METADATA_IP",
}

// childEnv builds the environment for one agent process: the base list, the
// configured provider credentials, and the variables the enabled MCP servers
// refer to.
//
// Names are resolved against the worker's environment; a name that is not set
// is left out rather than passed as empty, because opencode substitutes an
// unset variable with an empty string and an empty credential fails in a way
// nobody can read.
func (r *Runner) childEnv(servers map[string]MCPServer) []string {
	wanted := make(map[string]bool, len(baseEnv))
	for _, name := range baseEnv {
		wanted[name] = true
	}
	for _, name := range r.cfg.EnvPassthrough {
		if name = strings.TrimSpace(name); name != "" {
			wanted[name] = true
		}
	}
	for _, server := range servers {
		for _, name := range server.envRefs() {
			wanted[name] = true
		}
	}

	names := make([]string, 0, len(wanted))
	for name := range wanted {
		names = append(names, name)
	}
	// Sorted so that two runs of the same job produce the same environment,
	// which is one less thing to wonder about when one of them misbehaves.
	sort.Strings(names)

	env := make([]string, 0, len(names)+len(r.cfg.Env)+2)
	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	// Last, so that a deliberately configured value wins over an inherited
	// one of the same name.
	return append(env, r.cfg.Env...)
}
