package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"

	"github.com/yteraoka/kibitz/internal/scale"
)

// reconciler is the one thing the HTTP handler needs from a [scale.Scaler].
type reconciler interface {
	Reconcile(ctx context.Context) (scale.Result, error)
}

// reconcileResponse is what POST /reconcile answers with. It is read by
// whoever looks at Cloud Scheduler's attempt log, so it carries the decision
// and not just a status.
type reconcileResponse struct {
	Backlog int64  `json:"backlog"`
	Known   bool   `json:"backlog_known"`
	Current int    `json:"current"`
	Desired int    `json:"desired"`
	Reason  string `json:"reason"`
	Changed bool   `json:"changed"`
	Error   string `json:"error,omitempty"`
}

// reconcileHandler runs one reconciliation per request.
//
// This is how the scaler runs on Cloud Run: as a service that Cloud Scheduler
// calls, billed only while a request is in flight, rather than as a job whose
// every run starts a container and is billed for all of it. One reconcile is a
// read and at most one write, so the start-up was most of what was paid for.
//
// Requests are taken one at a time. A retry from Cloud Scheduler can overlap
// the attempt it retries, and two reconciles writing the instance count at
// once would each act on a count the other is changing.
type reconcileHandler struct {
	scaler reconciler
	logger *slog.Logger
	mu     sync.Mutex
}

func (h *reconcileHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()

	result, err := h.scaler.Reconcile(r.Context())
	resp := reconcileResponse{
		Backlog: result.Backlog.Undelivered,
		Known:   result.Backlog.Known,
		Current: result.Current,
		Desired: result.Desired,
		Reason:  string(result.Reason),
		Changed: result.Changed,
	}
	status := http.StatusOK
	if err != nil {
		// A failure has to reach Cloud Scheduler as one: it is what the
		// attempt log and the alert on this service count.
		resp.Error = err.Error()
		status = http.StatusInternalServerError
		h.logger.LogAttrs(r.Context(), slog.LevelError, "could not reconcile the worker scale",
			slog.String("result", result.String()),
			slog.String("error", err.Error()),
		)
	} else {
		h.logger.LogAttrs(r.Context(), slog.LevelInfo, "reconciled", slog.String("result", result.String()))
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}
