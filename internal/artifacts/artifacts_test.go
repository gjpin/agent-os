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
