// Package logx configures the process-wide slog handler.
//
// Every log line touching a document carries doc_id; every request-scoped
// line carries request_id. The two together are enough to grep one
// document's whole life out of a shared log.
package logx

import (
	"context"
	"io"
	"log/slog"
	"strings"
)

type ctxKey struct{ name string }

var (
	requestIDKey = ctxKey{"request_id"}
	docIDKey     = ctxKey{"doc_id"}
)

// Setup returns a JSON slog.Logger writing to w at the given level.
// Level strings: debug, info, warn, error. Unknown => info.
func Setup(w io.Writer, level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})
	return slog.New(&ctxHandler{Handler: h})
}

// WithRequestID returns a context that carries request_id on log lines.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// WithDocID returns a context that carries doc_id on log lines.
func WithDocID(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, docIDKey, id)
}

// RequestID returns the request_id carried on ctx, or "" if absent.
func RequestID(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}

// ctxHandler is a slog.Handler that decorates records with values pulled
// from the context. It is the reason every log line touching a document
// gets doc_id "for free" — callers only need to plumb the context.
type ctxHandler struct{ slog.Handler }

func (h *ctxHandler) Handle(ctx context.Context, r slog.Record) error {
	if v, _ := ctx.Value(requestIDKey).(string); v != "" {
		r.AddAttrs(slog.String("request_id", v))
	}
	if v, _ := ctx.Value(docIDKey).(int64); v != 0 {
		r.AddAttrs(slog.Int64("doc_id", v))
	}
	return h.Handler.Handle(ctx, r)
}

func (h *ctxHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return &ctxHandler{Handler: h.Handler.WithAttrs(a)}
}
func (h *ctxHandler) WithGroup(name string) slog.Handler {
	return &ctxHandler{Handler: h.Handler.WithGroup(name)}
}
