// Command ai is the single static binary for the AI Development Platform.
//
// All logic lives in internal/*; this entrypoint only runs the command tree
// and propagates its exit code (CLI spec §18).
package main

import (
	"os"

	"github.com/jt-helsinki/stack-genie/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
