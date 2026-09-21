// Package telemetry sets up structured logging. Every record passes through a
// handler that strips anything resembling a credential, because kibitz logs
// content it does not control: webhook payloads, git output and agent stderr.
package telemetry

import (
	"context"
	"io"
	"log/slog"
)

// Options configures the logger.
type Options struct {
	Level   slog.Level
	Format  string // json (default) or text
	Service string
	Version string
}

// New builds a logger writing to w.
func New(w io.Writer, opts Options) *slog.Logger {
	handlerOpts := &slog.HandlerOptions{Level: opts.Level}

	var h slog.Handler
	if opts.Format == "text" {
		h = slog.NewTextHandler(w, handlerOpts)
	} else {
		h = slog.NewJSONHandler(w, handlerOpts)
	}

	logger := slog.New(&redactHandler{inner: h})
	if opts.Service != "" {
		logger = logger.With(slog.String("service", opts.Service))
	}
	if opts.Version != "" {
		logger = logger.With(slog.String("version", opts.Version))
	}
	return logger
}

// Setup builds a logger and installs it as the slog default.
func Setup(w io.Writer, opts Options) *slog.Logger {
	logger := New(w, opts)
	slog.SetDefault(logger)
	return logger
}

// redactHandler applies [Redact] to the message and to every string attribute
// before handing the record to the wrapped handler.
type redactHandler struct {
	inner slog.Handler
}

func (h *redactHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *redactHandler) Handle(ctx context.Context, r slog.Record) error {
	clone := slog.NewRecord(r.Time, r.Level, Redact(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		clone.AddAttrs(redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, clone)
}

func (h *redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, redactAttr(a))
	}
	return &redactHandler{inner: h.inner.WithAttrs(out)}
}

func (h *redactHandler) WithGroup(name string) slog.Handler {
	return &redactHandler{inner: h.inner.WithGroup(name)}
}

func redactAttr(a slog.Attr) slog.Attr {
	// Resolve first so that types implementing slog.LogValuer, such as
	// config.Secret, get the chance to hide themselves.
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, Redact(v.String()))
	case slog.KindGroup:
		attrs := v.Group()
		out := make([]any, 0, len(attrs))
		for _, sub := range attrs {
			out = append(out, redactAttr(sub))
		}
		return slog.Group(a.Key, out...)
	case slog.KindAny:
		if err, ok := v.Any().(error); ok {
			return slog.String(a.Key, Redact(err.Error()))
		}
		if s, ok := v.Any().(interface{ String() string }); ok {
			return slog.String(a.Key, Redact(s.String()))
		}
		return slog.Attr{Key: a.Key, Value: v}
	default:
		return slog.Attr{Key: a.Key, Value: v}
	}
}
