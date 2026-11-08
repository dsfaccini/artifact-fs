package logging

import (
	"fmt"
	"io"
	"log/slog"

	"github.com/cloudflare/artifact-fs/internal/auth"
)

// NewJSONLogger returns a logger that redacts every string, error and
// fmt.Stringer value with auth.RedactString, at any group depth and after a
// slog.LogValuer resolves. It does not look inside a struct, slice or map
// value: log one only when none of its strings can hold a credential.
func NewJSONLogger(w io.Writer, level slog.Level) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			// Redact all string values unconditionally. The cost is negligible
			// vs. the risk of leaking tokens through substring heuristics.
			switch a.Value.Kind() {
			case slog.KindString:
				a.Value = slog.StringValue(auth.RedactString(a.Value.String()))
			case slog.KindAny:
				if e, ok := a.Value.Any().(error); ok {
					a.Value = slog.StringValue(auth.RedactString(e.Error()))
				} else if s, ok := a.Value.Any().(fmt.Stringer); ok {
					a.Value = slog.StringValue(auth.RedactString(s.String()))
				}
			}
			return a
		},
	})
	return slog.New(h)
}
