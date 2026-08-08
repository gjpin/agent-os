package releases

import (
	"strings"
	"testing"
)

func TestOrcaInstallScriptIsPinned(t *testing.T) {
	script, err := OrcaInstallScript("x86_64")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "sha256sum") || !strings.Contains(script, "github.com/stablyai/orca/releases/download/v1.4.176") {
		t.Fatalf("unpinned Orca install script: %s", script)
	}
}
