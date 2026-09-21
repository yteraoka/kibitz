package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Lookup resolves an environment variable. It mirrors [os.LookupEnv] so that
// tests can supply a fixed environment.
type Lookup func(key string) (string, bool)

// OSEnv reads from the process environment.
func OSEnv(key string) (string, bool) { return os.LookupEnv(key) }

// MapEnv builds a [Lookup] over a map. Useful in tests.
func MapEnv(m map[string]string) Lookup {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

// loader reads typed values out of the environment while collecting every
// problem it finds, so that a misconfigured deployment reports all of its
// missing settings at once instead of one per restart.
type loader struct {
	env  Lookup
	errs []error
}

func newLoader(env Lookup) *loader {
	if env == nil {
		env = OSEnv
	}
	return &loader{env: env}
}

func (l *loader) fail(key string, format string, args ...any) {
	l.errs = append(l.errs, fmt.Errorf("%s: %s", key, fmt.Sprintf(format, args...)))
}

// raw returns the trimmed value and whether it was set to anything non-empty.
func (l *loader) raw(key string) (string, bool) {
	v, ok := l.env(key)
	if !ok {
		return "", false
	}
	v = strings.TrimSpace(v)
	return v, v != ""
}

func (l *loader) str(key, def string) string {
	if v, ok := l.raw(key); ok {
		return v
	}
	return def
}

// requireIf records an error when cond holds and the value is empty. It is how
// backend-specific settings are validated (e.g. a topic is only required when
// the Pub/Sub backend is selected).
func (l *loader) requireIf(cond bool, key, value, because string) {
	if cond && value == "" {
		l.fail(key, "is required %s", because)
	}
}

func (l *loader) secret(key string) Secret {
	return Secret(l.str(key, ""))
}

func (l *loader) int(key string, def int) int {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		l.fail(key, "must be an integer, got %q", v)
		return def
	}
	return n
}

func (l *loader) positiveInt(key string, def int) int {
	n := l.int(key, def)
	if n <= 0 {
		l.fail(key, "must be greater than 0, got %d", n)
		return def
	}
	return n
}

func (l *loader) int64(key string, def int64) int64 {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		l.fail(key, "must be an integer, got %q", v)
		return def
	}
	return n
}

func (l *loader) bool(key string, def bool) bool {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		l.fail(key, "must be a boolean, got %q", v)
		return def
	}
	return b
}

func (l *loader) duration(key string, def time.Duration) time.Duration {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.fail(key, "must be a duration such as 15m, got %q", v)
		return def
	}
	if d <= 0 {
		l.fail(key, "must be greater than 0, got %q", v)
		return def
	}
	return d
}

// durationOrZero is like duration but accepts 0 as a meaningful value,
// for settings where zero means "disabled".
func (l *loader) durationOrZero(key string, def time.Duration) time.Duration {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.fail(key, "must be a duration such as 15m, got %q", v)
		return def
	}
	if d < 0 {
		l.fail(key, "must not be negative, got %q", v)
		return def
	}
	return d
}

// list splits a comma-separated value, dropping empty entries.
func (l *loader) list(key string, def []string) []string {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

// secrets reads a comma-separated list. Several values are allowed so that a
// secret can be rotated without downtime: every one of them is accepted while
// the old value is being retired.
func (l *loader) secrets(key string) []Secret {
	values := l.list(key, nil)
	out := make([]Secret, 0, len(values))
	for _, v := range values {
		out = append(out, Secret(v))
	}
	return out
}

func (l *loader) enum(key, def string, allowed ...string) string {
	v := l.str(key, def)
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	l.fail(key, "must be one of %s, got %q", strings.Join(allowed, ", "), v)
	return def
}

func (l *loader) logLevel(key string, def slog.Level) slog.Level {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(v)); err != nil {
		l.fail(key, "must be one of debug, info, warn, error, got %q", v)
		return def
	}
	return lvl
}
