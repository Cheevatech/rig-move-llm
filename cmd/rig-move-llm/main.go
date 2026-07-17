// Command rig-move-llm moves the heavy lifting off your paid LLM: a single static
// binary that proxies Claude Code's traffic, routing worker (subagent) inference to
// your own endpoint while the main agent stays on Anthropic direct.
package main

import (
	"os"

	"github.com/rigmovellm/rig-move-llm/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
