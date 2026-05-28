package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "opengate: no subcommand specified")
	os.Exit(2)
}
