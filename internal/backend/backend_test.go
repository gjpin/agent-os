package backend

import (
	"context"
	"testing"

	"github.com/zero/agent-os/internal/execx"
)

func TestLimaUsesArgumentArrays(t *testing.T) {
	runner := &execx.RecordingRunner{}
	provider := Lima{Runner: runner}
	if err := provider.Start(context.Background(), "agents"); err != nil {
		t.Fatal(err)
	}
	if len(runner.Calls) != 1 || runner.Calls[0].Name != "limactl" {
		t.Fatalf("unexpected calls: %+v", runner.Calls)
	}
	for _, arg := range runner.Calls[0].Args {
		if arg == "sh" || arg == "-c" || arg == "eval" {
			t.Fatalf("shell execution leaked into provider args: %+v", runner.Calls[0].Args)
		}
	}
}
