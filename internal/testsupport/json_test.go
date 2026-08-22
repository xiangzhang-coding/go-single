package testsupport

import (
	"fmt"
	"strings"
	"testing"
)

type recordingAssertionT struct {
	errors []string
}

func (*recordingAssertionT) Helper() {}

func (t *recordingAssertionT) Fatalf(format string, args ...any) {
	t.errors = append(t.errors, fmt.Sprintf(format, args...))
}

func TestAssertExactJSONKeys(t *testing.T) {
	t.Run("accepts exact keys regardless of order", func(t *testing.T) {
		recorder := &recordingAssertionT{}
		AssertExactJSONKeys(recorder, map[string]any{"name": "alice", "id": float64(1)}, "id", "name")
		if len(recorder.errors) != 0 {
			t.Fatalf("unexpected errors: %v", recorder.errors)
		}
	})

	t.Run("rejects missing or additional keys", func(t *testing.T) {
		for _, got := range []map[string]any{
			{"id": float64(1)},
			{"id": float64(1), "name": "alice", "role": "user"},
		} {
			recorder := &recordingAssertionT{}
			AssertExactJSONKeys(recorder, got, "id", "name")
			if len(recorder.errors) != 1 || !strings.Contains(recorder.errors[0], "want exactly") {
				t.Fatalf("errors = %v, want one exact-key failure", recorder.errors)
			}
		}
	})
}

func TestAssertAPIError(t *testing.T) {
	t.Run("accepts non-empty error and optional exact message", func(t *testing.T) {
		for _, message := range [][]string{nil, {"not found"}} {
			recorder := &recordingAssertionT{}
			AssertAPIError(recorder, map[string]any{"error": "not found"}, message...)
			if len(recorder.errors) != 0 {
				t.Fatalf("unexpected errors: %v", recorder.errors)
			}
		}
	})

	t.Run("rejects malformed envelopes", func(t *testing.T) {
		tests := []struct {
			name string
			body map[string]any
			want string
		}{
			{name: "missing error", body: map[string]any{}, want: "JSON keys"},
			{name: "additional key", body: map[string]any{"error": "bad", "code": 400}, want: "JSON keys"},
			{name: "wrong type", body: map[string]any{"error": 400}, want: "non-empty string"},
			{name: "empty message", body: map[string]any{"error": ""}, want: "non-empty string"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				recorder := &recordingAssertionT{}
				AssertAPIError(recorder, tt.body)
				if len(recorder.errors) != 1 || !strings.Contains(recorder.errors[0], tt.want) {
					t.Fatalf("errors = %v, want one failure containing %q", recorder.errors, tt.want)
				}
			})
		}
	})

	t.Run("rejects a different exact message", func(t *testing.T) {
		recorder := &recordingAssertionT{}
		AssertAPIError(recorder, map[string]any{"error": "not found"}, "forbidden")
		if len(recorder.errors) != 1 || !strings.Contains(recorder.errors[0], `want "forbidden"`) {
			t.Fatalf("errors = %v, want exact-message failure", recorder.errors)
		}
	})
}
