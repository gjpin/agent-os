package main

import (
	"os"
	"testing"
)

func TestAgentInstructionsAreEmbeddedExactly(t *testing.T) {
	tests := []struct {
		path string
		got  string
	}{
		{"internal/instructions/fedora/AGENTS.md", fedoraAgentInstructions},
		{"internal/instructions/debian/AGENTS.md", debianAgentInstructions},
	}
	for _, tc := range tests {
		want, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if tc.got != string(want) {
			t.Fatalf("embedded agent instructions differ from %s", tc.path)
		}
	}
}
