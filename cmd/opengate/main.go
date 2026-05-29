// Command opengate is the OpenGate access-control SaaS executable.
//
// The binary exposes operational modes through subcommands. Currently
// only "migrate" is implemented; api, worker, simulator, and bootstrap
// arrive in later epics. Running without a subcommand is a usage error.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "opengate: no subcommand specified")
		os.Exit(2)
	}

	switch os.Args[1] {
	case "migrate":
		if err := runMigrate(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "opengate:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "opengate: unknown subcommand %q\n", os.Args[1])
		os.Exit(2)
	}
}
