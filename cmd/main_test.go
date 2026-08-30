package main

import "testing"

func TestSubcommandRequired(t *testing.T) {
	if err := run([]string{"per-user-container-operator"}); err == nil {
		t.Fatal("expected error when no subcommand is given")
	}
}

func TestUnknownSubcommandRejected(t *testing.T) {
	if err := run([]string{"per-user-container-operator", "nope"}); err == nil {
		t.Fatal("expected error for unknown subcommand")
	}
}
