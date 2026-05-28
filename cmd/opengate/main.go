// Command opengate is the OpenGate access-control SaaS executable.
//
// The binary will eventually expose multiple operational modes through
// subcommands (api, worker, simulator, bootstrap, migrate). Running
// without any subcommand produces a usage error and exits with status 2,
// following the conventional Unix exit code for command-line misuse.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "opengate: no subcommand specified")
	os.Exit(2)
}
