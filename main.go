package main

import (
	_ "embed"

	"github.com/gjpin/agent-os/internal/cli"
)

// The shared instruction source is part of the release input. Embedding it
// here keeps installed binaries independent of the directory from which they
// are invoked.
//
//go:embed internal/instructions/fedora/AGENTS.md
var fedoraAgentInstructions string

//go:embed internal/instructions/debian/AGENTS.md
var debianAgentInstructions string

func main() {
	cli.ExecuteWithInstructions(cli.AgentInstructions{
		Fedora: fedoraAgentInstructions,
		Debian: debianAgentInstructions,
	})
}
