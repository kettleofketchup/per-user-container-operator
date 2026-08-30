package main

import (
	"bytes"
	"io"
	"os"
	"testing"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns
// whatever fn wrote to it.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	runErr := fn()

	if closeErr := w.Close(); closeErr != nil {
		t.Fatalf("close pipe writer: %v", closeErr)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return buf.String(), runErr
}

// TestUserkeySubcommandContract asserts the CLI contract Task 16's migration
// Job depends on: correct arity writes exactly the 18-char key plus a
// trailing newline to stdout and exits 0; any other arity writes nothing to
// stdout and returns a non-nil error (cmd/main.go turns that into a non-zero
// exit and a usage line on stderr).
func TestUserkeySubcommandContract(t *testing.T) {
	out, err := captureStdout(t, func() error {
		return run([]string{"per-user-container-operator", "userkey", "open-webui", "workspace-app", "alice"})
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "u-e7f5a178dc8eb805\n" {
		t.Fatalf("stdout = %q, want %q", out, "u-e7f5a178dc8eb805\n")
	}
}

func TestUserkeySubcommandRejectsWrongArity(t *testing.T) {
	cases := [][]string{
		{"per-user-container-operator", "userkey", "open-webui", "workspace-app"},
		{"per-user-container-operator", "userkey", "open-webui", "workspace-app", "alice", "extra"},
	}
	for _, argv := range cases {
		out, err := captureStdout(t, func() error {
			return run(argv)
		})
		if err == nil {
			t.Fatalf("argv=%v: expected error, got nil", argv)
		}
		if out != "" {
			t.Fatalf("argv=%v: stdout = %q, want empty", argv, out)
		}
	}
}
