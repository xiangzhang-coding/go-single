package testsupport

import "sort"

type assertionT interface {
	Helper()
	Fatalf(format string, args ...any)
}

// AssertExactJSONKeys checks that a JSON object has exactly the expected top-level keys.
func AssertExactJSONKeys(t assertionT, got map[string]any, want ...string) {
	t.Helper()
	assertExactJSONKeys(t, got, want)
}

// AssertAPIError checks the shared API error envelope. When supplied, message
// must contain exactly one expected error message.
func AssertAPIError(t assertionT, got map[string]any, message ...string) {
	t.Helper()
	if len(message) > 1 {
		t.Fatalf("AssertAPIError accepts at most one expected message, got %d", len(message))
		return
	}
	if !assertExactJSONKeys(t, got, []string{"error"}) {
		return
	}

	errMessage, ok := got["error"].(string)
	if !ok || errMessage == "" {
		t.Fatalf("API error field = %#v, want non-empty string", got["error"])
		return
	}
	if len(message) == 1 && errMessage != message[0] {
		t.Fatalf("API error message = %q, want %q", errMessage, message[0])
	}
}

func assertExactJSONKeys(t assertionT, got map[string]any, want []string) bool {
	gotKeys := make([]string, 0, len(got))
	for key := range got {
		gotKeys = append(gotKeys, key)
	}
	wantKeys := append([]string(nil), want...)
	sort.Strings(gotKeys)
	sort.Strings(wantKeys)
	if !equalStrings(gotKeys, wantKeys) {
		t.Fatalf("JSON keys = %v, want exactly %v", gotKeys, wantKeys)
		return false
	}
	return true
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
