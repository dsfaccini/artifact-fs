package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/auth"
)

type credentialStringer string

func (s credentialStringer) String() string { return string(s) }

type credentialValuer string

func (v credentialValuer) LogValue() slog.Value { return slog.StringValue(string(v)) }

func TestNewJSONLoggerRedactsEveryStringBoundary(t *testing.T) {
	secret := "https://credential-user:super-secret@example.com/org/repo.git"
	want := auth.RedactString(secret)
	var output bytes.Buffer
	logger := NewJSONLogger(&output, slog.LevelInfo)
	logger.Info(secret,
		"string", secret,
		"error", errors.New(secret),
		"stringer", credentialStringer(secret),
		"valuer", credentialValuer(secret),
		slog.Group("group", "string", secret, slog.Group("nested", "error", errors.New(secret))),
		"attempt", 2,
	)

	for _, exposed := range []string{"credential-user", "super-secret"} {
		if strings.Contains(output.String(), exposed) {
			t.Fatalf("logger exposed %q: %s", exposed, output.String())
		}
	}
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("decode JSON log: %v\n%s", err, output.String())
	}
	for _, key := range []string{"msg", "string", "error", "stringer", "valuer"} {
		if got := record[key]; got != want {
			t.Fatalf("%s = %v, want %q", key, got, want)
		}
	}
	wantGroup := map[string]any{"string": want, "nested": map[string]any{"error": want}}
	if got := record["group"]; !reflect.DeepEqual(got, wantGroup) {
		t.Fatalf("group = %v, want %v", got, wantGroup)
	}
	if got := record["attempt"]; got != float64(2) {
		t.Fatalf("attempt = %v, want 2", got)
	}
}

// The logger does not look inside a struct, slice or map value. The JSON
// handler marshals one as it is, so a credential in it reaches the log.
func TestNewJSONLoggerLeavesCompositeValuesAsTheyAre(t *testing.T) {
	secret := "https://credential-user:super-secret@example.com/org/repo.git"
	var output bytes.Buffer
	NewJSONLogger(&output, slog.LevelInfo).Info("composite",
		"slice", []string{secret},
		"map", map[string]string{"remote": secret},
		"struct", struct{ Remote string }{secret},
	)
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("decode JSON log: %v\n%s", err, output.String())
	}
	for key, want := range map[string]any{
		"slice":  []any{secret},
		"map":    map[string]any{"remote": secret},
		"struct": map[string]any{"Remote": secret},
	} {
		if got := record[key]; !reflect.DeepEqual(got, want) {
			t.Fatalf("%s = %v, want %v", key, got, want)
		}
	}
}
