// Command per-user-container-operator dispatches to the controller, router,
// and userkey subcommands.
package main

import (
	"errors"
	"fmt"
	"os"
)

func run(argv []string) error {
	if len(argv) < 2 {
		return errors.New("usage: per-user-container-operator <controller|router|userkey>")
	}
	switch argv[1] {
	// controller wired in Task 11, router in Task 10, userkey in Task 3.
	// userkey exists so the migration Job (Task 16) never reimplements the
	// frozen derivation in a script: two implementations is total silent data
	// loss.
	case "controller", "router", "userkey":
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q", argv[1])
	}
}

func main() {
	if err := run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
