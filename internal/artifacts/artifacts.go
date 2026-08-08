package artifacts

import (
	"crypto/sha256"
	"encoding/xml"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/zero/agent-os/internal/model"
	"github.com/zero/agent-os/internal/provision"
	"github.com/zero/agent-os/internal/releases"
)

type VMDefinition struct {
	Name           string
	CPUs           int
	MemoryMiB      int
	DiskGiB        int
	Architecture   string
	AccessMode     model.AccessMode
	OrcaPort       int
	Packages       []string
	ImagePath      string
	BindAddress    string
	PairingAddress string
	GuestAddress   string
	MACAddress     string
	SecurityModel  string
	AllowedCIDRs   []string
}

func FromConfig(c model.Config, architecture string) VMDefinition {
	packages := append([]string(nil), c.Packages...)
	sort.Strings(packages)
	pairingAddress := "127.0.0.1"
	if c.AccessMode == model.AccessWireGuard && c.WireGuardAddress != "" {
		pairingAddress = addressHostPart(c.WireGuardAddress)
	}
	return VMDefinition{
		Name: c.VMName, CPUs: c.VMCPUs, MemoryMiB: c.VMMemoryMiB,
		DiskGiB: c.VMDiskGiB, Architecture: architecture,
		AccessMode: c.AccessMode, OrcaPort: c.OrcaPort, Packages: packages,
		// The WireGuard address is on the host, not in the guest. The host
		// forwarding layer selects the externally reachable address while
		// Orca listens on the guest NIC.
		BindAddress:    "0.0.0.0",
		PairingAddress: pairingAddress,
		GuestAddress:   LibvirtGuestAddress(c.VMName),
		MACAddress:     LibvirtMACAddress(c.VMName),
		AllowedCIDRs:   append([]string(nil), c.AllowedCIDRs...),
	}
}

// LibvirtXML deliberately omits graphics, audio, USB, clipboard, agent
// channels, virtiofs/9p, and host socket sharing. The disk path and network
// name are provider-owned values, not user-interpolated shell fragments.
func LibvirtXML(def VMDefinition, diskPath, cloudInitPath string) (string, error) {
	if def.Name == "" || diskPath == "" || cloudInitPath == "" {
		return "", fmt.Errorf("name, disk path, and cloud-init path are required")
	}
	if def.CPUs < 1 || def.CPUs > 8 || def.MemoryMiB < 512 || def.DiskGiB < 20 {
		return "", fmt.Errorf("invalid VM resources")
	}
	name, disk, seed := xmlEscape(def.Name), xmlEscape(diskPath), xmlEscape(cloudInitPath)
	mac := ""
	if def.MACAddress != "" {
		mac = fmt.Sprintf("      <mac address=\"%s\"/>\n", xmlEscape(def.MACAddress))
	}
	return fmt.Sprintf(`<domain type="kvm">
  <name>%s</name>
  <memory unit="MiB">%d</memory>
  <currentMemory unit="MiB">%d</currentMemory>
  <vcpu placement="static" current="%d">%d</vcpu>
  <resource>
    <partition>/machine.slice/agent-os.slice</partition>
  </resource>
	<features>
	    <acpi/>
	    <vmport state="off"/>
	  </features>
	  <seclabel type="dynamic" model="%s" relabel="yes"/>
  <cpu mode="host-passthrough" check="none"/>
  <os>
    <type arch="%s" machine="q35">hvm</type>
    <boot dev="hd"/>
  </os>
  <clock offset="utc"/>
  <on_poweroff>destroy</on_poweroff>
  <on_reboot>restart</on_reboot>
  <on_crash>destroy</on_crash>
  <devices>
    <disk type="file" device="disk">
      <driver name="qemu" type="qcow2" discard="unmap"/>
      <source file="%s"/>
      <target dev="vda" bus="virtio"/>
    </disk>
    <disk type="file" device="cdrom">
      <driver name="qemu" type="raw"/>
      <source file="%s"/>
      <target dev="sda" bus="sata"/>
      <readonly/>
    </disk>
    <interface type="network">
%s      <source network="agent-os-nat"/>
      <model type="virtio"/>
      <driver name="qemu" iommu="on"/>
    </interface>
    <console type="pty"><target type="serial" port="0"/></console>
    <rng model="virtio"><backend model="random">/dev/urandom</backend></rng>
    <memballoon model="virtio"/>
    <serial type="pty"><target type="isa-serial" port="0"/></serial>
  </devices>
</domain>
`, name, def.MemoryMiB, def.MemoryMiB, def.CPUs, def.CPUs, securityModel(def.SecurityModel), architecture(def.Architecture), mac, disk, seed), nil
}

func architecture(value string) string {
	if value == "" {
		return "x86_64"
	}
	return value
}

func securityModel(value string) string {
	if value == "apparmor" {
		return "apparmor"
	}
	return "selinux"
}

func memorySize(mib int) string {
	if mib%1024 == 0 {
		return fmt.Sprintf("%dGiB", mib/1024)
	}
	return fmt.Sprintf("%dMiB", mib)
}

func xmlEscape(value string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(value))
	return b.String()
}

func LimaYAML(def VMDefinition) (string, error) {
	if def.Name == "" {
		return "", fmt.Errorf("VM name is required")
	}
	orcaInstallScript, err := releases.OrcaInstallScript(def.Architecture)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "arch: %s\nvmType: vz\nplain: true\nrosetta: false\ncontainerd: false\n", architecture(def.Architecture))
	fmt.Fprintf(&b, "cpus: %d\nmemory: %s\ndisk: %dGiB\n", def.CPUs, memorySize(def.MemoryMiB), def.DiskGiB)
	if def.ImagePath != "" {
		fmt.Fprintf(&b, "images:\n  - location: %s\n    arch: %s\n", strconv.Quote(def.ImagePath), architecture(def.Architecture))
	}
	b.WriteString("mountType: none\nmounts: []\nssh: true\nsshLoadDotSSHPubKeys: false\n\nprovision:\n  - mode: system\n    script: |")
	b.WriteString("\n      set -eu\n      useradd --create-home --shell /bin/bash agent || true\n")
	b.WriteString("      usermod --lock agent || true\n")
	b.WriteString("      systemctl disable --now containerd.service 2>/dev/null || true\n")
	b.WriteString("      cat > /run/agent-os-install-orca <<'AGENT_OS_ORCA_INSTALL'\n")
	appendIndented(&b, orcaInstallScript, "      ")
	b.WriteString("      AGENT_OS_ORCA_INSTALL\n      /bin/bash /run/agent-os-install-orca\n")
	b.WriteString("      install -d -m 0755 /etc/agent-os\n      cat > /etc/systemd/system/orca.service <<'AGENT_OS_ORCA_UNIT'\n")
	appendIndented(&b, OrcaSystemdUnit(def.OrcaPort, def.BindAddress, def.PairingAddress), "      ")
	b.WriteString("      AGENT_OS_ORCA_UNIT\n")
	b.WriteString("      cat > /etc/agent-os/firewall.rules <<'AGENT_OS_FIREWALL'\n")
	appendIndented(&b, strings.Join(FirewallRules(def.AllowedCIDRs, def.OrcaPort), "\n"), "      ")
	b.WriteString("      AGENT_OS_FIREWALL\n      cat > /etc/systemd/system/agent-os-firewall.service <<'AGENT_OS_FIREWALL_UNIT'\n")
	appendIndented(&b, FirewallSystemdUnit(), "      ")
	b.WriteString("      AGENT_OS_FIREWALL_UNIT\n      systemctl daemon-reload\n      systemctl enable --now agent-os-firewall.service\n      systemctl enable --now orca.service\n")
	return b.String(), nil
}

func CloudInit(def VMDefinition, repositoryKeyPath string) string {
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	b.WriteString("users:\n  - name: agent\n    gecos: Agent\n    groups: []\n    shell: /bin/bash\n    lock_passwd: true\n    sudo: []\n")
	b.WriteString("ssh_pwauth: false\npackages:\n")
	if err := provision.ValidatePackages(def.Packages); err != nil {
		return "#cloud-config\n# invalid package manifest: " + err.Error() + "\n"
	}
	for _, pkg := range def.Packages {
		fmt.Fprintf(&b, "  - %s\n", pkg)
	}
	b.WriteString("write_files:\n")
	b.WriteString("  - path: /etc/agent-os/README\n    permissions: '0644'\n    content: |\n      Managed by agent-os. Credentials are provisioned separately.\n")
	if repositoryKeyPath != "" {
		// This is a guest destination, never the host key content or a host
		// path. The private key is copied only by the explicit provisioning
		// workflow after correspondence and permission checks.
		b.WriteString("  - path: /etc/agent-os/repository-key-source\n    permissions: '0600'\n    content: |\n      Provisioning source is operator-supplied; key material is not in cloud-init.\n")
	}
	b.WriteString("  - path: /etc/systemd/system/orca.service\n    permissions: '0644'\n    content: |\n")
	appendIndented(&b, OrcaSystemdUnit(def.OrcaPort, def.BindAddress, def.PairingAddress), "      ")
	b.WriteString("  - path: /etc/agent-os/firewall.rules\n    permissions: '0600'\n    content: |\n")
	appendIndented(&b, strings.Join(FirewallRules(def.AllowedCIDRs, def.OrcaPort), "\n"), "      ")
	orcaInstallScript, err := releases.OrcaInstallScript(def.Architecture)
	if err != nil {
		return "#cloud-config\n# invalid Orca architecture: " + err.Error() + "\n"
	}
	b.WriteString("  - path: /usr/local/libexec/agent-os-install-orca\n    permissions: '0700'\n    content: |\n")
	appendIndented(&b, orcaInstallScript, "      ")
	b.WriteString("  - path: /etc/systemd/system/agent-os-firewall.service\n    permissions: '0644'\n    content: |\n")
	appendIndented(&b, FirewallSystemdUnit(), "      ")
	b.WriteString("runcmd:\n  - [bash, /usr/local/libexec/agent-os-install-orca]\n")
	b.WriteString("  - [systemctl, daemon-reload]\n")
	b.WriteString("  - [systemctl, enable, --now, agent-os-firewall.service]\n")
	b.WriteString("  - [systemctl, enable, --now, orca.service]\n")
	return b.String()
}

func appendIndented(b *strings.Builder, value, prefix string) {
	for _, line := range strings.Split(strings.TrimSuffix(value, "\n"), "\n") {
		b.WriteString(prefix + line + "\n")
	}
}

func OrcaSystemdUnit(port int, bindAddress string, pairingAddress ...string) string {
	pairing := bindAddress
	if len(pairingAddress) > 0 && strings.TrimSpace(pairingAddress[0]) != "" {
		pairing = pairingAddress[0]
	}
	return fmt.Sprintf(`[Unit]
Description=Orca headless remote server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=agent
Group=agent
ExecStart=/usr/bin/orca serve --port %d --pairing-address %s
Environment=ORCA_MODE=headless
Environment=ORCA_PORT=%d
Environment=ORCA_BIND_ADDRESS=%s
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/home/agent

[Install]
WantedBy=multi-user.target
`, port, pairing, port, bindAddress)
}

func LibvirtNetworkXML(definitions ...VMDefinition) string {
	dhcpHost := ""
	if len(definitions) > 0 && definitions[0].MACAddress != "" && definitions[0].GuestAddress != "" {
		dhcpHost = fmt.Sprintf("    <host mac=\"%s\" name=\"%s\" ip=\"%s\"/>\n",
			xmlEscape(definitions[0].MACAddress), xmlEscape(definitions[0].Name), xmlEscape(definitions[0].GuestAddress))
	}
	return fmt.Sprintf(`<network>
  <name>agent-os-nat</name>
  <forward mode="nat"/>
  <bridge name="agent-os0" stp="on" delay="0"/>
  <domain name="agent-os.internal" localOnly="yes"/>
  <ip address="192.168.240.1" netmask="255.255.255.0">
    <dhcp><range start="192.168.240.10" end="192.168.240.240"/>
%s    </dhcp>
  </ip>
</network>
`, dhcpHost)
}

func LimaPortForward(c model.Config) string {
	hostIP := "127.0.0.1"
	if c.AccessMode == model.AccessWireGuard {
		hostIP = c.WireGuardAddress
		if slash := strings.IndexByte(hostIP, '/'); slash >= 0 {
			hostIP = hostIP[:slash]
		}
	}
	return fmt.Sprintf("portForwards:\n  - guestPort: %d\n    hostPort: %d\n    hostIP: %s\n    guestIP: 0.0.0.0\n    guestIPMustBeZero: false\n    static: true\n", c.OrcaPort, c.OrcaPort, strconv.Quote(hostIP))
}

func FirewallRules(allowedCIDRs []string, orcaPort ...int) []string {
	rules := []string{
		"nft add table inet agent_os",
		"nft add chain inet agent_os forward { type filter hook forward priority 0; policy drop; }",
		"nft add rule inet agent_os forward ct state established,related accept",
		"nft add chain inet agent_os output { type filter hook output priority 0; policy drop; }",
		"nft add rule inet agent_os output ct state established,related accept",
		"nft add rule inet agent_os output oifname \"eth0\" udp dport { 53, 67, 68, 123 } accept",
		"nft add rule inet agent_os output oifname \"eth0\" tcp dport 53 accept",
		"nft add rule inet agent_os output oifname \"eth0\" ip daddr != { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 169.254.0.0/16 } accept",
		"nft add rule inet agent_os output oifname \"eth0\" ip6 daddr != { ::1/128, fc00::/7, fe80::/10 } accept",
		"nft add rule inet agent_os output oifname \"eth0\" ip6 daddr fc00::/7 drop",
		"nft add chain inet agent_os input { type filter hook input priority 0; policy drop; }",
	}
	if len(orcaPort) > 0 && orcaPort[0] >= 1 && orcaPort[0] <= 65535 {
		rules = append(rules, fmt.Sprintf("nft add rule inet agent_os input iifname \"eth0\" tcp dport %d accept", orcaPort[0]))
	}
	for _, cidr := range allowedCIDRs {
		if strings.TrimSpace(cidr) != "" {
			rules = append(rules, fmt.Sprintf("nft add rule inet agent_os output oifname \"eth0\" ip daddr %s accept", cidr))
		}
	}
	return rules
}

// LibvirtGuestAddress and LibvirtMACAddress provide a stable DHCP reservation
// for each VM. Host forwarding must target the guest's address, and querying a
// lease after every boot would leave a window where the VM is unreachable.
func LibvirtGuestAddress(name string) string {
	digest := sha256.Sum256([]byte(name))
	return fmt.Sprintf("192.168.240.%d", 10+int(digest[0])%231)
}

func LibvirtMACAddress(name string) string {
	digest := sha256.Sum256([]byte(name))
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", digest[0], digest[1], digest[2])
}

func ForwardingTableName(name string) string {
	digest := sha256.Sum256([]byte(name))
	return fmt.Sprintf("agent_os_fwd_%x", digest[:6])
}

// LinuxForwardingRules returns an IPv4 nftables ruleset for the selected host
// endpoint. The libvirt network is IPv4-only, so reject an IPv6 WireGuard
// address explicitly instead of installing a rule that can never work.
func LinuxForwardingRules(c model.Config, guestAddress string) (string, error) {
	guestIP := net.ParseIP(addressHostPart(guestAddress)).To4()
	if guestIP == nil {
		return "", fmt.Errorf("libvirt guest address must be IPv4")
	}
	if c.OrcaPort < 1 || c.OrcaPort > 65535 {
		return "", fmt.Errorf("invalid Orca port %d", c.OrcaPort)
	}
	if c.AccessMode == model.AccessWireGuard {
		if !validInterfaceName(c.WireGuardInterface) {
			return "", fmt.Errorf("invalid WireGuard interface %q", c.WireGuardInterface)
		}
		if net.ParseIP(addressHostPart(c.WireGuardAddress)).To4() == nil {
			return "", fmt.Errorf("Linux forwarding requires an IPv4 WireGuard address")
		}
	}

	guest := guestIP.String()
	table := ForwardingTableName(c.VMName)
	lines := []string{
		fmt.Sprintf("table ip %s {", table),
		"  chain prerouting {",
		"    type nat hook prerouting priority -100; policy accept;",
	}
	if c.AccessMode == model.AccessWireGuard {
		lines = append(lines, fmt.Sprintf("    iifname %q ip daddr %s tcp dport %d dnat to %s:%d", c.WireGuardInterface, addressHostPart(c.WireGuardAddress), c.OrcaPort, guest, c.OrcaPort))
	}
	lines = append(lines,
		"  }",
		"  chain output {",
		"    type nat hook output priority -100; policy accept;",
	)
	if c.AccessMode == model.AccessLocal {
		lines = append(lines, fmt.Sprintf("    ip daddr 127.0.0.1 tcp dport %d dnat to %s:%d", c.OrcaPort, guest, c.OrcaPort))
	}
	lines = append(lines,
		"  }",
		"  chain postrouting {",
		"    type nat hook postrouting priority 100; policy accept;",
		fmt.Sprintf("    oifname \"agent-os0\" ip daddr %s tcp dport %d masquerade", guest, c.OrcaPort),
		"  }",
		"  chain forward {",
		"    type filter hook forward priority -50; policy accept;",
		"    ct state established,related accept",
	)
	if c.AccessMode == model.AccessWireGuard {
		lines = append(lines, fmt.Sprintf("    iifname %q oifname \"agent-os0\" ip daddr %s tcp dport %d accept", c.WireGuardInterface, guest, c.OrcaPort))
	} else {
		lines = append(lines, fmt.Sprintf("    ip saddr 127.0.0.1 oifname \"agent-os0\" ip daddr %s tcp dport %d accept", guest, c.OrcaPort))
	}
	lines = append(lines, "  }", "}", "")
	return strings.Join(lines, "\n"), nil
}

func addressHostPart(value string) string {
	if slash := strings.IndexByte(value, '/'); slash >= 0 {
		return value[:slash]
	}
	return value
}

func validInterfaceName(value string) bool {
	if len(value) == 0 || len(value) > 15 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' && r != '.' {
			return false
		}
	}
	return true
}

func FirewallSystemdUnit() string {
	return `[Unit]
Description=agent-os guest egress firewall
After=network-pre.target
Before=network.target orca.service

[Service]
Type=oneshot
ExecStart=/usr/sbin/nft -f /etc/agent-os/firewall.rules
RemainAfterExit=yes
ExecReload=/usr/sbin/nft -f /etc/agent-os/firewall.rules

[Install]
WantedBy=multi-user.target
`
}

func PackageManifest(packages []string) []string {
	result := append([]string(nil), packages...)
	sort.Strings(result)
	return result
}
