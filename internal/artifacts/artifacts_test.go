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
	if !strings.Contains(guestFirewall, `input iifname "eth0" tcp dport 6768 accept`) {
		t.Fatalf("guest firewall does not permit forwarded Orca traffic: %s", guestFirewall)
	}
}
