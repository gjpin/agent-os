package artifacts

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gjpin/agent-os/internal/model"
	"github.com/gjpin/agent-os/internal/provision"
	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
)

type cloudInitFile struct {
	Path        string `yaml:"path"`
	Owner       string `yaml:"owner"`
	Permissions string `yaml:"permissions"`
	Encoding    string `yaml:"encoding"`
	Content     string `yaml:"content"`
}

type cloudInitDocument struct {
	Packages   []string        `yaml:"packages"`
	WriteFiles []cloudInitFile `yaml:"write_files"`
}

type limaPortForward struct {
	GuestPort         int    `yaml:"guestPort"`
	HostPort          int    `yaml:"hostPort"`
	HostIP            string `yaml:"hostIP"`
	GuestIP           string `yaml:"guestIP"`
	GuestIPMustBeZero bool   `yaml:"guestIPMustBeZero"`
	Static            bool   `yaml:"static"`
}

type limaProvision struct {
	Script string `yaml:"script"`
}

type limaDocument struct {
	SSH          map[string]bool   `yaml:"ssh"`
	PortForwards []limaPortForward `yaml:"portForwards"`
	Provision    []limaProvision   `yaml:"provision"`
}

type libvirtDomain struct {
	Devices struct {
		Channels []struct {
			Type   string `xml:"type,attr"`
			Target struct {
				Type string `xml:"type,attr"`
				Name string `xml:"name,attr"`
			} `xml:"target"`
		} `xml:"channel"`
	} `xml:"devices"`
}

func writeArtifactTestKey(t *testing.T, dir string) (string, []byte) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "artifact-test")
	if err != nil {
		t.Fatal(err)
	}
	privateData := pem.EncodeToMemory(block)
	path := filepath.Join(dir, "operator", "repository", "id_ed25519")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, privateData, 0o600); err != nil {
		t.Fatal(err)
	}
	publicKey, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".pub", ssh.MarshalAuthorizedKey(publicKey), 0o644); err != nil {
		t.Fatal(err)
	}
	return path, privateData
}

func TestGeneratedArtifactsDisableHostSharing(t *testing.T) {
	def := VMDefinition{Name: "agents", CPUs: 2, MemoryMiB: 4096, DiskGiB: 120, Architecture: "x86_64", Packages: []string{"git"}}
	libvirtXML, err := LibvirtXML(def, "/state/disk.qcow2", "/state/cloud-init.iso")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"<graphics", "<audio", "virtiofs", "9p", "clipboard", "ssh-agent"} {
		if strings.Contains(strings.ToLower(libvirtXML), strings.ToLower(forbidden)) {
			t.Fatalf("generated XML contains %q", forbidden)
		}
	}
	lima, err := LimaYAML(def)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lima, "vmType: vz") || !strings.Contains(lima, "plain: true") || !strings.Contains(lima, "rosetta:\n  enabled: false\n  binfmt: false") || !strings.Contains(lima, "containerd:\n  system: false\n  user: false") {
		t.Fatalf("Lima hardening missing: %s", lima)
	}
	if strings.Contains(lima, "mountType:") || strings.Contains(lima, "rosetta: false") || strings.Contains(lima, "containerd: false") {
		t.Fatalf("Lima profile contains a legacy/incompatible scalar schema: %s", lima)
	}
	keyPath, _ := writeArtifactTestKey(t, t.TempDir())
	cloudInit := CloudInit(def, keyPath)
	if strings.Contains(cloudInit, keyPath) || !strings.Contains(cloudInit, "name: agent") {
		t.Fatal("cloud-init leaked host key path or omitted agent")
	}
	for _, expected := range []string{
		"<channel type=\"unix\">",
		"org.qemu.guest_agent.0",
	} {
		if !strings.Contains(libvirtXML, expected) {
			t.Errorf("libvirt XML omits %q", expected)
		}
	}
	for _, inert := range []string{"agent-os:hardening", "agent-os:seccomp", "agent-os:mount-namespace", "agent-os:cgroup-devices", "qemu:commandline"} {
		if strings.Contains(libvirtXML, inert) {
			t.Errorf("libvirt XML contains inert or duplicate hardening annotation %q", inert)
		}
	}
	if err := xml.Unmarshal([]byte(libvirtXML), &struct{}{}); err != nil {
		t.Fatalf("generated libvirt XML is not well-formed: %v", err)
	}
	var domain libvirtDomain
	if err := xml.Unmarshal([]byte(libvirtXML), &domain); err != nil {
		t.Fatalf("decode generated libvirt XML: %v", err)
	}
	if len(domain.Devices.Channels) != 1 || domain.Devices.Channels[0].Type != "unix" || domain.Devices.Channels[0].Target.Type != "virtio" || domain.Devices.Channels[0].Target.Name != "org.qemu.guest_agent.0" {
		t.Fatalf("unexpected libvirt channels: %+v", domain.Devices.Channels)
	}
	var limaProfile limaDocument
	if err := yaml.Unmarshal([]byte(lima+LimaPortForward(model.DefaultConfig("/state"))), &limaProfile); err != nil {
		t.Fatalf("generated Lima YAML is not valid YAML: %v", err)
	}
	if value, ok := limaProfile.SSH["loadDotSSHPubKeys"]; !ok || value {
		t.Fatalf("Lima SSH loadDotSSHPubKeys schema/value is wrong: %+v", limaProfile.SSH)
	}
	if value, ok := limaProfile.SSH["forwardAgent"]; !ok || value {
		t.Fatalf("Lima SSH forwardAgent schema/value is wrong: %+v", limaProfile.SSH)
	}
	if len(limaProfile.PortForwards) != 1 || !limaProfile.PortForwards[0].Static {
		t.Fatalf("Lima plain-mode forwarding is not static: %+v", limaProfile.PortForwards)
	}
	_ = model.AccessLocal
}

func TestLibvirtXMLSecurityModels(t *testing.T) {
	for _, test := range []struct {
		model string
		want  string
	}{
		{model: "dac", want: `<seclabel type="dynamic" model="dac" relabel="yes"/>`},
		{model: "apparmor", want: `<seclabel type="dynamic" model="apparmor" relabel="yes"/>`},
		{model: "selinux", want: `<seclabel type="dynamic" model="selinux" relabel="yes"/>`},
	} {
		t.Run(test.model, func(t *testing.T) {
			xmlDefinition, err := LibvirtXML(VMDefinition{
				Name: "agents", CPUs: 2, MemoryMiB: 4096, DiskGiB: 120, SecurityModel: test.model,
			}, "/state/disk.qcow2", "/state/cloud-init.iso")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(xmlDefinition, test.want) {
				t.Fatalf("generated XML omits security label %q", test.want)
			}
		})
	}
}

func TestRepositoryKeyProvisioningUsesGuestOnlyPathsAndSafeMetadata(t *testing.T) {
	keyPath, privateData := writeArtifactTestKey(t, t.TempDir())
	def := VMDefinition{
		Name: "agents", CPUs: 2, MemoryMiB: 4096, DiskGiB: 120,
		Architecture: "x86_64", RepositoryKeyPath: keyPath,
	}
	wantEncoded := base64.StdEncoding.EncodeToString(privateData)

	cloudInit, err := CloudInitWithError(def, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"/etc/agent-os/keys/id_ed25519",
		"owner: root:root",
		"permissions: '0600'",
		"encoding: b64",
		wantEncoded,
		"qemu-guest-agent",
	} {
		if !strings.Contains(cloudInit, expected) {
			t.Errorf("cloud-init omits %q", expected)
		}
	}
	if strings.Contains(cloudInit, keyPath) || strings.Contains(cloudInit, string(privateData)) {
		t.Fatal("cloud-init leaked a host key path or raw private-key bytes")
	}
	if strings.Contains(cloudInit, "repository-key-source") || strings.Contains(cloudInit, "key material is not included") {
		t.Fatal("cloud-init still emits the placeholder provisioning marker")
	}
	var cloudInitData cloudInitDocument
	if err := yaml.Unmarshal([]byte(cloudInit), &cloudInitData); err != nil {
		t.Fatalf("cloud-init is not valid YAML: %v", err)
	}
	var foundCloudKey bool
	for _, file := range cloudInitData.WriteFiles {
		if file.Path != "/etc/agent-os/keys/id_ed25519" {
			continue
		}
		foundCloudKey = true
		if file.Owner != "root:root" || file.Permissions != "0600" || file.Encoding != "b64" {
			t.Fatalf("cloud-init key metadata is unsafe: %+v", file)
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(file.Content))
		if err != nil {
			t.Fatalf("decode cloud-init key content: %v", err)
		}
		if string(decoded) != string(privateData) {
			t.Fatal("cloud-init changed the private-key bytes")
		}
	}
	if !foundCloudKey {
		t.Fatal("cloud-init did not write the repository key")
	}

	lima, err := LimaYAML(def)
	if err != nil {
		t.Fatal(err)
	}
	var limaDocument map[string]any
	if err := yaml.Unmarshal([]byte(lima), &limaDocument); err != nil {
		t.Fatalf("Lima profile is not valid YAML: %v", err)
	}
	sshConfig, ok := limaDocument["ssh"].(map[string]any)
	if !ok || sshConfig["loadDotSSHPubKeys"] != false || sshConfig["forwardAgent"] != false {
		t.Fatalf("Lima profile has the wrong v2 SSH object: %#v", limaDocument["ssh"])
	}
	for _, legacyField := range []string{"sshLoadDotSSHPubKeys"} {
		if _, ok := limaDocument[legacyField]; ok {
			t.Fatalf("Lima profile contains legacy SSH field %q", legacyField)
		}
	}
	for _, expected := range []string{
		"ssh:\n  loadDotSSHPubKeys: false\n  forwardAgent: false",
		"/etc/agent-os/keys/id_ed25519",
		"install -d -o root -g root -m 0700 /etc/agent-os/keys",
		"chown root:root '/etc/agent-os/keys/id_ed25519'",
		wantEncoded,
	} {
		if !strings.Contains(lima, expected) {
			t.Errorf("Lima profile omits %q", expected)
		}
	}
	var foundLimaKey bool
	for _, provision := range limaDocument["provision"].([]any) {
		entry := provision.(map[string]any)
		if script, ok := entry["script"].(string); ok && strings.Contains(script, wantEncoded) {
			foundLimaKey = true
		}
	}
	if !foundLimaKey {
		t.Fatal("Lima provisioning script did not include the validated key material")
	}
	if strings.Contains(lima, keyPath) || strings.Contains(lima, string(privateData)) {
		t.Fatal("Lima profile leaked a host key path or raw private-key bytes")
	}
}

func TestCloudInitDoesNotEchoInvalidHostKeyPath(t *testing.T) {
	def := VMDefinition{Name: "agents", CPUs: 2, MemoryMiB: 4096, DiskGiB: 120, Architecture: "x86_64"}
	hostPath := filepath.Join(t.TempDir(), "private", "id_ed25519")
	cloudInit := CloudInit(def, hostPath)
	if strings.Contains(cloudInit, hostPath) {
		t.Fatal("cloud-init echoed an invalid host key path")
	}
	if !strings.Contains(cloudInit, "repository private key was not provisioned") {
		t.Fatal("cloud-init did not record safe provisioning failure state")
	}
	if _, err := CloudInitWithError(def, hostPath); err == nil {
		t.Fatal("error-returning cloud-init accepted a missing private key")
	}
}

func TestFirewallRulesAreNftScriptCommands(t *testing.T) {
	rules := FirewallRules([]string{"10.20.0.0/16", "not a cidr"}, 6768)
	if len(rules) == 0 || rules[0] != "add table inet agent_os" {
		t.Fatalf("unexpected nft script start: %v", rules)
	}
	for _, line := range rules {
		if strings.HasPrefix(line, "nft ") {
			t.Fatalf("nft executable leaked into ruleset line %q", line)
		}
	}
	joined := strings.Join(rules, "\n")
	for _, expected := range []string{
		"add chain inet agent_os input",
		"add rule inet agent_os input iifname != \"lo\" tcp dport 6768 accept",
		"add rule inet agent_os output oifname != \"lo\" ip daddr 10.20.0.0/16 accept",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("firewall omits %q: %s", expected, joined)
		}
	}
	if strings.Contains(joined, "not a cidr") {
		t.Fatal("firewall emitted an unvalidated CIDR")
	}
	checked, err := FirewallRulesChecked([]string{"10.20.0.0/16\naccept", "2001:db8::/32"}, 6768)
	if err == nil || checked != nil {
		t.Fatalf("checked firewall generation accepted an injected CIDR: rules=%v err=%v", checked, err)
	}
	checked, err = FirewallRulesChecked([]string{"2001:db8::/32"}, 6768)
	if err != nil || !strings.Contains(strings.Join(checked, "\n"), "ip6 daddr 2001:db8::/32 accept") {
		t.Fatalf("checked firewall generation mishandled IPv6 CIDR: rules=%v err=%v", checked, err)
	}
}

func TestAgentInstructionsAreIncludedInProviderArtifacts(t *testing.T) {
	content := "# shared instructions\n\nKeep this exact.\n"
	def := VMDefinition{
		Name: "agents", CPUs: 2, MemoryMiB: 4096, DiskGiB: 120,
		Architecture: "x86_64", AgentInstructions: content,
	}
	cloudInit := CloudInit(def, "")
	lima, err := LimaYAML(def)
	if err != nil {
		t.Fatal(err)
	}
	for name, artifact := range map[string]string{"cloud-init": cloudInit, "Lima": lima} {
		for _, line := range []string{"# shared instructions", "Keep this exact."} {
			if !strings.Contains(artifact, line) {
				t.Errorf("%s omits embedded instruction line %q", name, line)
			}
		}
		for _, expected := range []string{
			provision.AgentInstructionsCanonicalPath,
			provision.AgentInstructionsOpencodePath,
			provision.AgentInstructionsCodexPath,
			provision.AgentInstructionsClaudePath,
			provision.AgentInstructionsPiPath,
			provision.AgentInstructionsCopilotPath,
			"rm -rf --",
			"ln -s --",
		} {
			if !strings.Contains(artifact, expected) {
				t.Errorf("%s omits setup detail %q", name, expected)
			}
		}
	}
	if !strings.Contains(cloudInit, "/usr/local/libexec/agent-os-provision-agent-instructions") {
		t.Fatal("cloud-init does not install and run the setup script")
	}
	if !strings.Contains(lima, "/run/agent-os-provision-agent-instructions") {
		t.Fatal("Lima does not run the setup script")
	}
}

func TestCodingAgentsInstallerIsOrderedBeforeOrcaForEveryProvider(t *testing.T) {
	def := VMDefinition{Name: "agents", CPUs: 2, MemoryMiB: 4096, DiskGiB: 120, Architecture: "x86_64", Packages: []string{"htop", "git"}}
	cloudInit := CloudInit(def, "")
	lima, err := LimaYAML(def)
	if err != nil {
		t.Fatal(err)
	}
	var cloudDoc cloudInitDocument
	if err := yaml.Unmarshal([]byte(cloudInit), &cloudDoc); err != nil {
		t.Fatalf("cloud-init is not valid YAML: %v", err)
	}
	cloudPackages := make(map[string]bool, len(cloudDoc.Packages))
	for _, pkg := range cloudDoc.Packages {
		cloudPackages[pkg] = true
	}
	for _, pkg := range append(provision.BaselinePackages(), "htop") {
		if !cloudPackages[pkg] {
			t.Errorf("cloud-init package manifest omits %q", pkg)
		}
	}
	if !cloudPackages["qemu-guest-agent"] {
		t.Error("cloud-init omits the libvirt qemu guest agent")
	}
	var limaDoc limaDocument
	if err := yaml.Unmarshal([]byte(lima), &limaDoc); err != nil {
		t.Fatalf("Lima profile is not valid YAML: %v", err)
	}
	if len(limaDoc.Provision) != 1 {
		t.Fatalf("unexpected Lima provisioning entries: %+v", limaDoc.Provision)
	}
	limaScript := limaDoc.Provision[0].Script
	for _, pkg := range append(provision.BaselinePackages(), "htop") {
		if !strings.Contains(limaScript, " "+pkg+" ") && !strings.Contains(limaScript, " "+pkg+"\n") {
			t.Errorf("Lima package install omits %q", pkg)
		}
	}
	if strings.Contains(limaScript, " qemu-guest-agent ") || strings.Contains(limaScript, " qemu-guest-agent\n") {
		t.Error("Lima includes the libvirt-only qemu guest agent")
	}
	for name, artifact := range map[string]string{"cloud-init": cloudInit, "Lima": lima} {
		for _, command := range []string{
			"dnf install -y -- dnf-plugins-core",
			"dnf config-manager addrepo --from-repofile=https://rpm.releases.hashicorp.com/fedora/hashicorp.repo",
			"dnf install -y -- terraform",
			"dnf install -y -- adoptium-temurin-java-repository",
			"dnf config-manager setopt adoptium.enabled=1",
			"dnf install -y -- temurin-25-jdk",
		} {
			if !strings.Contains(artifact, command) {
				t.Errorf("%s artifact omits Temurin provisioning command %q", name, command)
			}
		}
		instructions := strings.Index(artifact, "agent-os-provision-agent-instructions")
		packageInstall := strings.Index(artifact, "dnf install -y -- ")
		agents := strings.Index(artifact, "agent-os-install-coding-agents")
		orca := strings.Index(artifact, "agent-os-install-orca")
		if name == "Lima" && (packageInstall < 0 || !(instructions < packageInstall && packageInstall < agents)) {
			t.Fatalf("Lima package provisioning order is instructions=%d packages=%d agents=%d", instructions, packageInstall, agents)
		}
		if instructions < 0 || agents < 0 || orca < 0 || !(instructions < agents && agents < orca) {
			t.Fatalf("%s provisioning order is instructions=%d agents=%d orca=%d", name, instructions, agents, orca)
		}
		for _, endpoint := range []string{
			"https://opencode.ai/install",
			"https://chatgpt.com/codex/install.sh",
			"https://claude.ai/install.sh",
			"https://gh.io/copilot-install",
		} {
			if !strings.Contains(artifact, endpoint) {
				t.Errorf("%s artifact omits %s", name, endpoint)
			}
		}
	}
}

func TestForwardingArtifactsUseHostEndpoint(t *testing.T) {
	c := model.DefaultConfig("/state")
	c.VMName = "agents"
	c.OrcaPort = 6768
	c.AccessMode = model.AccessWireGuard
	c.WireGuardInterface = "wg0"
	c.WireGuardAddress = "10.64.0.2/32"
	def := FromConfig(c, "x86_64")

	if def.BindAddress != "0.0.0.0" || def.PairingAddress != "10.64.0.2" {
		t.Fatalf("unexpected guest/host addresses: bind=%q pairing=%q", def.BindAddress, def.PairingAddress)
	}
	unit := OrcaSystemdUnit(def.OrcaPort, def.BindAddress, def.PairingAddress)
	if !strings.Contains(unit, "--pairing-address 10.64.0.2") || !strings.Contains(unit, "ORCA_BIND_ADDRESS=0.0.0.0") {
		t.Fatalf("Orca unit does not separate pairing and bind addresses: %s", unit)
	}

	yaml := LimaPortForward(c)
	for _, expected := range []string{"hostIP: \"10.64.0.2\"", "guestIP: 0.0.0.0", "guestIPMustBeZero: false", "static: true"} {
		if !strings.Contains(yaml, expected) {
			t.Fatalf("Lima forwarding missing %q: %s", expected, yaml)
		}
	}

	rules, err := LinuxForwardingRules(c, def.GuestAddress)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"iifname \"wg0\"", "ip daddr 10.64.0.2", "dnat to " + def.GuestAddress + ":6768", "oifname \"agent-os0\""} {
		if !strings.Contains(rules, expected) {
			t.Fatalf("Linux forwarding missing %q: %s", expected, rules)
		}
	}

	network := LibvirtNetworkXML(def)
	if !strings.Contains(network, `mac="`+def.MACAddress+`"`) || !strings.Contains(network, `ip="`+def.GuestAddress+`"`) {
		t.Fatalf("libvirt DHCP reservation missing: %s", network)
	}
	guestFirewall := strings.Join(FirewallRules(nil, c.OrcaPort), "\n")
	if !strings.Contains(guestFirewall, `input iifname != "lo" tcp dport 6768 accept`) {
		t.Fatalf("guest firewall does not permit forwarded Orca traffic: %s", guestFirewall)
	}
}
