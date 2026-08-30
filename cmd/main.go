// Command per-user-container-operator dispatches to the controller, router,
// and userkey subcommands.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/kettleofketchup/per-user-container-operator/internal/identity"
)

func run(argv []string) error {
	if len(argv) < 2 {
		return errors.New("usage: per-user-container-operator <controller|router|userkey>")
	}
	switch argv[1] {
	// controller wired in Task 11, router in Task 10.
	case "controller", "router":
		return nil
	case "userkey":
		if len(argv) != 5 {
			return errors.New("usage: per-user-container-operator userkey <namespace> <appName> <rawIdentity>")
		}
		fmt.Println(identity.UserKey(argv[2], argv[3], argv[4]))
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
