package main

import (
	_ "embed"

	"github.com/zero/agent-os/internal/cli"
)

// The shared instruction source is part of the release input. Embedding it
// here keeps installed binaries independent of the directory from which they
// are invoked.
//
//go:embed internal/instructions/AGENTS.md
var agentInstructions string

func main() {
	cli.Execute(agentInstructions)
}
