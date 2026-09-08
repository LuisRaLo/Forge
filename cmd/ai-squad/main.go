// Command ai-squad is a local orchestrator for a squad of autonomous software
// development agents.
//
// Concrete runtimes and providers are wired here, at the edge of the program,
// so that the orchestration core depends only on interfaces and never on any
// particular vendor.
package main

import (
	"os"

	"github.com/LuisRaLo/ai-squad/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
