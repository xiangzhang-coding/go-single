package testsupport

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

type fakeTestHandle struct {
	fatal string
	skip  string
}

func (*fakeTestHandle) Helper() {}

func (f *fakeTestHandle) Fatalf(format string, args ...any) {
	f.fatal = fmt.Sprintf(format, args...)
}

func (f *fakeTestHandle) Skipf(format string, args ...any) {
	f.skip = fmt.Sprintf(format, args...)
}

func TestRequireDependency(t *testing.T) {
	errUnavailable := errors.New("connection refused")
	tests := []struct {
		name      string
		required  string
		err       error
		wantFatal bool
		wantSkip  bool
	}{
		{name: "available", required: "1"},
		{name: "optional locally", err: errUnavailable, wantSkip: true},
		{name: "required in CI", required: "1", err: errUnavailable, wantFatal: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(integrationRequiredEnv, tt.required)
			fake := &fakeTestHandle{}
			RequireDependency(fake, "Redis", tt.err)

			if got := fake.fatal != ""; got != tt.wantFatal {
				t.Fatalf("fatal = %v, want %v (%q)", got, tt.wantFatal, fake.fatal)
			}
			if got := fake.skip != ""; got != tt.wantSkip {
				t.Fatalf("skip = %v, want %v (%q)", got, tt.wantSkip, fake.skip)
			}
			message := fake.fatal + fake.skip
			if tt.err != nil && !strings.Contains(message, "connection refused") {
				t.Fatalf("message %q does not include dependency error", message)
			}
		})
	}
}
