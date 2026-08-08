package main

import (
	"os"
	"testing"
)

func TestAgentInstructionsAreEmbeddedExactly(t *testing.T) {
	want, err := os.ReadFile("internal/instructions/AGENTS.md")
	if err != nil {
		t.Fatal(err)
	}
	if agentInstructions != string(want) {
		t.Fatal("embedded agent instructions differ from internal/instructions/AGENTS.md")
	}
}
