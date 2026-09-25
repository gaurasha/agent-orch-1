// Package obs provides structured logging and metrics.
//
// Both are hand-rolled on the standard library. log/slog gives us structured
// logging for free; Prometheus exposition is a text format simple enough that
// pulling in client_golang (and its transitive tree) is not worth it for the
// dozen series this system emits. If we later need histograms with exemplars,
// native histograms or the collector ecosystem, that trade flips.
package obs

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

type Logger struct{ *slog.Logger }

// NewLogger returns a JSON logger. JSON rather than text because these logs are
// going to a collector, and because an agent's tool arguments routinely contain
// newlines that would corrupt a line-oriented format.
func NewLogger(service, level string) *Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv})
	// Tagged "service", not "component": individual subsystems add their own
	// "component" field, and using the same key at both levels produces
	// duplicate keys in the JSON output.
	return &Logger{slog.New(h).With("service", service)}
}

// With returns a logger carrying extra fields, keeping the concrete type so
// callers are not forced to handle *slog.Logger.
func (l *Logger) With(args ...any) *Logger { return &Logger{l.Logger.With(args...)} }

// WithRun returns a logger that tags every line with the run and tenant.
// Operator question "what is agent X doing right now" is answered by grepping
// one field, not by correlating across services.
func (l *Logger) WithRun(runID, tenantID string) *Logger {
	return l.With("run_id", runID, "tenant_id", tenantID)
}

func (l *Logger) Ctx(ctx context.Context) *Logger {
	if id, ok := ctx.Value(traceKey{}).(string); ok {
		return l.With("trace_id", id)
	}
	return l
}

type traceKey struct{}

// WithTrace attaches a trace id to a context so log lines from the worker, the
// gateway and the sandbox can be stitched together for one tool call.
func WithTrace(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceKey{}, id)
}

func TraceID(ctx context.Context) string {
	id, _ := ctx.Value(traceKey{}).(string)
	return id
}
