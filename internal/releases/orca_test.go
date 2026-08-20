package releases

import (
	"strings"
	"testing"

	"github.com/gjpin/agent-os/internal/provision"
)

func TestOrcaInstallScriptIsPinned(t *testing.T) {
	script, err := OrcaInstallScript(provision.DistributionFedora, "x86_64")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "sha256sum") || !strings.Contains(script, "github.com/stablyai/orca/releases/download/v1.4.176") {
		t.Fatalf("unpinned Orca install script: %s", script)
	}
}

func TestOrcaInstallScriptSelectsDebianPackage(t *testing.T) {
	script, err := OrcaInstallScript(provision.DistributionDebian, "aarch64")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"orca-ide_1.4.176_arm64.deb", "apt-get install -y", "10f5974c42076e50"} {
		if !strings.Contains(script, want) {
			t.Fatalf("Debian Orca install script omits %q: %s", want, script)
		}
	}
}

func TestOrcaLatestInstallScriptVerifiesPublishedDigest(t *testing.T) {
	for _, tc := range []struct {
		distribution provision.Distribution
		architecture string
		assetPart    string
	}{
		{provision.DistributionFedora, "x86_64", `x86_64\.rpm`},
		{provision.DistributionDebian, "aarch64", `arm64\.deb`},
	} {
		script, err := OrcaLatestInstallScript(tc.distribution, tc.architecture)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"releases/latest", tc.assetPart, `startswith("sha256:")`, `test "$actual" = "$expected"`} {
			if !strings.Contains(script, want) {
				t.Errorf("latest Orca installer omits %q", want)
			}
		}
	}
}
