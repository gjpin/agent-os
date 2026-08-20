package artifacts

import (
	"strings"
	"testing"

	"github.com/gjpin/agent-os/internal/model"
)

func TestLimaYAMLUsesExplicitHostDriver(t *testing.T) {
	for _, driver := range []string{"qemu", "vz"} {
		def := FromConfig(model.Config{VMName: "agents", VMCPUs: 4, VMMemoryMiB: 4096, VMDiskGiB: 40, OrcaPort: 7777}, "x86_64")
		def.VMType = driver
		got, err := LimaYAML(def)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"vmType: " + driver, "plain: true", "mounts: []", "static: true", "containerd:\n  system: false", "rosetta:\n  enabled: false"} {
			if !strings.Contains(got+LimaPortForward(model.Config{OrcaPort: 7777}), want) {
				t.Errorf("%s YAML omits %q", driver, want)
			}
		}
	}
}

func TestLimaYAMLRejectsUnknownDriver(t *testing.T) {
	def := VMDefinition{Name: "agents", VMType: "other", Architecture: "x86_64"}
	if _, err := LimaYAML(def); err == nil {
		t.Fatal("unknown driver accepted")
	}
}

func TestLimaYAMLComposesSharedSetupOnceForEachDistro(t *testing.T) {
	for _, distribution := range []model.Distribution{model.DistributionFedora, model.DistributionDebian} {
		def := FromConfig(model.Config{VMName: "agents", VMCPUs: 2, VMMemoryMiB: 4096, VMDiskGiB: 40, OrcaPort: 6768}, "x86_64", distribution)
		def.VMType = "qemu"
		got, err := LimaYAML(def)
		if err != nil {
			t.Fatal(err)
		}
		for _, marker := range []string{"cat > /run/agent-os-install-coding-agents <<'AGENT_OS_CODING_AGENTS'", "cat > /run/agent-os-install-orca <<'AGENT_OS_ORCA_INSTALL'", "cat > /run/agent-os-setup-kind-podman <<'AGENT_OS_KIND_PODMAN'"} {
			if strings.Count(got, marker) != 1 {
				t.Errorf("%s artifact contains %q %d times", distribution, marker, strings.Count(got, marker))
			}
		}
		if distribution == model.DistributionDebian {
			if !strings.Contains(got, "apt.releases.hashicorp.com trixie") || strings.Contains(got, "rpmdevtools") {
				t.Fatalf("unexpected Debian distro setup")
			}
		} else if !strings.Contains(got, "rpm.releases.hashicorp.com/fedora") {
			t.Fatalf("Fedora distro setup omitted HashiCorp repository")
		}
	}
}
