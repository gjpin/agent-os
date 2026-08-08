package artifacts

import (
	"strings"
	"testing"

	"github.com/zero/agent-os/internal/model"
)

func TestGeneratedArtifactsDisableHostSharing(t *testing.T) {
	def := VMDefinition{Name: "agents", CPUs: 2, MemoryMiB: 4096, DiskGiB: 120, Architecture: "x86_64", Packages: []string{"git"}}
	xml, err := LibvirtXML(def, "/state/disk.qcow2", "/state/cloud-init.iso")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"<graphics", "<audio", "virtiofs", "9p", "clipboard", "ssh-agent"} {
		if strings.Contains(strings.ToLower(xml), strings.ToLower(forbidden)) {
			t.Fatalf("generated XML contains %q", forbidden)
		}
	}
	lima, err := LimaYAML(def)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lima, "vmType: vz") || !strings.Contains(lima, "plain: true") || !strings.Contains(lima, "rosetta: false") {
		t.Fatalf("Lima hardening missing: %s", lima)
	}
	cloudInit := CloudInit(def, "/secret/id_ed25519")
	if strings.Contains(cloudInit, "/secret/id_ed25519") || !strings.Contains(cloudInit, "name: agent") {
		t.Fatal("cloud-init leaked host key path or omitted agent")
	}
	_ = model.AccessLocal
}
