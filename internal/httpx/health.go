package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"
)

// CheckFunc reports whether a dependency is usable. It must return quickly;
// the readiness handler bounds it with a timeout regardless.
type CheckFunc func(context.Context) error

// Health serves liveness and readiness. Liveness only says the process is up;
// readiness runs the registered checks, so a component that cannot reach its
// queue stops receiving traffic instead of failing every request.
type Health struct {
	version string
	timeout time.Duration

	mu     sync.RWMutex
	checks map[string]CheckFunc
}

// NewHealth creates a health endpoint pair reporting the given version.
func NewHealth(version string) *Health {
	return &Health{
		version: version,
		timeout: 2 * time.Second,
		checks:  make(map[string]CheckFunc),
	}
}

// Register adds a readiness check. Registering the same name twice replaces it.
func (h *Health) Register(name string, fn CheckFunc) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.checks[name] = fn
}

type healthResponse struct {
	Status  string            `json:"status"`
	Version string            `json:"version,omitempty"`
	Checks  map[string]string `json:"checks,omitempty"`
}

// Live handles GET /healthz.
func (h *Health) Live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok", Version: h.version})
}

// Ready handles GET /readyz.
func (h *Health) Ready(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	names := make([]string, 0, len(h.checks))
	for name := range h.checks {
		names = append(names, name)
	}
	checks := make(map[string]CheckFunc, len(h.checks))
	for name, fn := range h.checks {
		checks[name] = fn
	}
	h.mu.RUnlock()
	sort.Strings(names)

	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()

	resp := healthResponse{Status: "ok", Version: h.version}
	status := http.StatusOK
	if len(names) > 0 {
		resp.Checks = make(map[string]string, len(names))
	}
	for _, name := range names {
		if err := checks[name](ctx); err != nil {
			resp.Checks[name] = err.Error()
			resp.Status = "unavailable"
			status = http.StatusServiceUnavailable
			continue
		}
		resp.Checks[name] = "ok"
	}
	writeJSON(w, status, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
