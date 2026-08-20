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
		for _, marker := range []string{"cat > /run/agent-os-install-coding-agents <<'AGENT_OS_CODING_AGENTS'", "cat > /run/agent-os-install-orca <<'AGENT_OS_ORCA_INSTALL'", "cat > /run/agent-os-install-chrome <<'AGENT_OS_CHROME'", "cat > /run/agent-os-setup-container-runtime <<'AGENT_OS_CONTAINER_RUNTIME'", "cat > /run/agent-os-install-k3s-cilium <<'AGENT_OS_K3S_CILIUM'"} {
			if strings.Count(got, marker) != 1 {
				t.Errorf("%s artifact contains %q %d times", distribution, marker, strings.Count(got, marker))
			}
		}
		if distribution == model.DistributionDebian {
			if !strings.Contains(got, "download.docker.com/linux/debian") || !strings.Contains(got, "google-chrome-stable_current_amd64.deb") || strings.Contains(got, "KIND_EXPERIMENTAL_PROVIDER") {
				t.Fatalf("unexpected Debian distro setup")
			}
		} else if !strings.Contains(got, "rpm.releases.hashicorp.com/fedora") || !strings.Contains(got, "google-chrome-stable_current_x86_64.rpm") || !strings.Contains(got, "agent-os-podman.conf") {
			t.Fatalf("Fedora distro setup omitted HashiCorp repository")
		}
		for _, want := range []string{"flannel-backend: none", "disable-network-policy: true", "cilium install --set operator.replicas=1", "@playwright/test@latest @playwright/cli@latest"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s artifact omits %q", distribution, want)
			}
		}
		if strings.Contains(got, "KIND_EXPERIMENTAL_PROVIDER") || strings.Contains(got, "helm kind kustomize") {
			t.Errorf("%s artifact retains kind provisioning", distribution)
		}
	}
}

func TestFirewallAllowsCiliumClusterTrafficWithoutOpeningPrivateEgress(t *testing.T) {
	rules, err := FirewallRulesChecked(nil, 6768)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(rules, "\n")
	for _, want := range []string{
		`iifname { "cilium_host", "cilium_net", "cilium_vxlan" }`,
		`iifname "lxc*"`,
		`ip daddr { 10.42.0.0/16, 10.43.0.0/16 } accept`,
		`ip daddr != { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 169.254.0.0/16 } accept`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("Cilium-aware firewall omits %q", want)
		}
	}
}
