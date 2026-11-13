package logging

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
)

// The guard handler is the log pipeline's safety net. The primary
// protection is by construction — audit records are built from a
// fixed field whitelist (see System.Audit) and services never pass
// business objects to the logger — but any attribute that still
// reaches the run/error logs passes through here:
//
//   - attributes whose key names a credential-bearing field are
//     dropped outright (never written in any form);
//   - error values are reduced to a scrubbed string (URL userinfo,
//     key=value secrets and long token-like runs removed, length
//     capped).
//
// Dropping is deliberate: a missing debug attribute is harmless,
// while a leaked credential in a log file — which diagnostics may
// later export — is not.

var deniedAttrKeys = map[string]bool{
	"password": true, "passwd": true, "pwd": true,
	"secret": true, "secrets": true, "secret_ref": true, "secretref": true,
	"token": true, "access_token": true, "refresh_token": true,
	"cookie": true, "cookies": true, "set-cookie": true,
	"url": true, "subscription_url": true, "proxy_url": true,
	"username": true, "user_name": true, "user": true,
	"auth": true, "authorization": true, "proxy_auth": true,
	"credential": true, "credentials": true,
	"config": true, "config_json": true, "configjson": true,
	"apikey": true, "api_key": true, "api-key": true,
	"vault": true, "envelope": true, "blob": true,
}

var (
	urlUserinfoRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^\s/@'"]+@`)
	kvSecretRe    = regexp.MustCompile(`(?i)(password|passwd|pwd|secret|token|api[-_ ]?key)\s*[:=]\s*[^\s,;]+`)
	longTokenRe   = regexp.MustCompile(`[A-Za-z0-9+/=_-]{48,}`)
)

// ScrubErrorText reduces an error message to a log-safe string.
func ScrubErrorText(s string) string {
	s = urlUserinfoRe.ReplaceAllString(s, "$1***@")
	s = kvSecretRe.ReplaceAllString(s, "$1=***")
	s = longTokenRe.ReplaceAllString(s, "***")
	if len(s) > 512 {
		s = s[:512] + "…"
	}
	return s
}

// sanitizeAttr filters one attribute; ok=false drops it.
func sanitizeAttr(a slog.Attr) (slog.Attr, bool) {
	key := strings.ToLower(a.Key)
	if deniedAttrKeys[key] {
		return slog.Attr{}, false
	}
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindGroup:
		var kept []slog.Attr
		for _, sub := range v.Group() {
			if sa, ok := sanitizeAttr(sub); ok {
				kept = append(kept, sa)
			}
		}
		if len(kept) == 0 {
			return slog.Attr{}, false
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(kept...)}, true
	case slog.KindAny:
		if err, isErr := v.Any().(error); isErr {
			return slog.Attr{Key: a.Key, Value: slog.StringValue(ScrubErrorText(err.Error()))}, true
		}
		return a, true
	case slog.KindString:
		if key == "error" || key == "err" {
			return slog.Attr{Key: a.Key, Value: slog.StringValue(ScrubErrorText(v.String()))}, true
		}
		return a, true
	default:
		return a, true
	}
}

func sanitizeAttrs(attrs []slog.Attr) []slog.Attr {
	var out []slog.Attr
	for _, a := range attrs {
		if sa, ok := sanitizeAttr(a); ok {
			out = append(out, sa)
		}
	}
	return out
}

// guardHandler wraps another handler and sanitizes every record.
type guardHandler struct {
	next slog.Handler
}

func newGuardHandler(next slog.Handler) *guardHandler { return &guardHandler{next: next} }

func (h *guardHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *guardHandler) Handle(ctx context.Context, r slog.Record) error {
	clean := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		if sa, ok := sanitizeAttr(a); ok {
			clean.AddAttrs(sa)
		}
		return true
	})
	return h.next.Handle(ctx, clean)
}

func (h *guardHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &guardHandler{next: h.next.WithAttrs(sanitizeAttrs(attrs))}
}

func (h *guardHandler) WithGroup(name string) slog.Handler {
	return &guardHandler{next: h.next.WithGroup(name)}
}

// teeHandler fans records out to the run handler and, for errors,
// additionally to the error-category handler.
type teeHandler struct {
	run, errs slog.Handler
	level     slog.Level
}

func (h *teeHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *teeHandler) Handle(ctx context.Context, r slog.Record) error {
	err := h.run.Handle(ctx, r)
	if r.Level >= slog.LevelError {
		if e2 := h.errs.Handle(ctx, r); err == nil {
			err = e2
		}
	}
	return err
}

func (h *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &teeHandler{run: h.run.WithAttrs(attrs), errs: h.errs.WithAttrs(attrs), level: h.level}
}

func (h *teeHandler) WithGroup(name string) slog.Handler {
	return &teeHandler{run: h.run.WithGroup(name), errs: h.errs.WithGroup(name), level: h.level}
}

// renameAttr maps slog's built-in keys onto the Stage 5B record
// schema: time → timestamp, msg → event.
func renameAttr(_ []string, a slog.Attr) slog.Attr {
	switch a.Key {
	case slog.TimeKey:
		a.Key = "timestamp"
	case slog.MessageKey:
		a.Key = "event"
	}
	return a
}
