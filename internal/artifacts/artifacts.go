package artifacts

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/gjpin/agent-os/internal/credentials"
	"github.com/gjpin/agent-os/internal/model"
	"github.com/gjpin/agent-os/internal/provision"
	"github.com/gjpin/agent-os/internal/releases"
)

type VMDefinition struct {
	Name              string
	CPUs              int
	MemoryMiB         int
	DiskGiB           int
	Architecture      string
	AccessMode        model.AccessMode
	OrcaPort          int
	Packages          []string
	Skills            []string
	ImagePath         string
	BindAddress       string
	PairingAddress    string
	GuestAddress      string
	MACAddress        string
	SecurityModel     string
	AllowedCIDRs      []string
	AgentInstructions string
	RepositoryKeyPath string
	ProfileBackend    string
	ProfileDiskID     string
	ProfileDiskLabel  string
	ProfileDiskFormat bool
	ProfileDiskPath   string
	ProfileDiskSerial string
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
		Skills: append([]string(nil), c.Skills...),
		// The WireGuard address is on the host, not in the guest. The host
		// forwarding layer selects the externally reachable address while
		// Orca listens on the guest NIC.
		BindAddress:       "0.0.0.0",
		PairingAddress:    pairingAddress,
		GuestAddress:      LibvirtGuestAddress(c.VMName),
		MACAddress:        LibvirtMACAddress(c.VMName),
		AllowedCIDRs:      append([]string(nil), c.AllowedCIDRs...),
		RepositoryKeyPath: c.RepositoryKeyPath,
	}
}

// LibvirtXML deliberately omits graphics, audio, USB passthrough, clipboard,
// virtiofs/9p, and host socket sharing. The disk path and network name are
// provider-owned values, not user-interpolated shell fragments. Libvirt's
// QEMU driver supplies the host-level seccomp, private-namespace, and device
// cgroup controls; the backend preflights qemu.conf so those defaults have not
// been explicitly disabled. The backend requires those settings to be present
// in qemu.conf and verifies libvirt's native QEMU translation before defining
// the domain. The QEMU guest-agent channel is the one host/guest control
// channel required by the Linux management backend.
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
	profileDisk := ""
	if def.ProfileDiskPath != "" {
		serial := def.ProfileDiskSerial
		if serial == "" {
			serial = def.ProfileDiskID
		}
		profileDisk = fmt.Sprintf(`
    <disk type="file" device="disk">
      <driver name="qemu" type="qcow2" discard="unmap"/>
      <source file="%s"/>
      <target dev="vdb" bus="virtio"/>
      <serial>%s</serial>
    </disk>`, xmlEscape(def.ProfileDiskPath), xmlEscape(serial))
	}
	return fmt.Sprintf(`<domain type="kvm">
  <name>%s</name>
  <memory unit="MiB">%d</memory>
  <currentMemory unit="MiB">%d</currentMemory>
  <vcpu placement="static" current="%d">%d</vcpu>
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
%s
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
    <controller type="usb" index="0" model="none"/>
    <channel type="unix">
      <target type="virtio" name="org.qemu.guest_agent.0"/>
    </channel>
    <console type="pty"><target type="serial" port="0"/></console>
    <rng model="virtio"><backend model="random">/dev/urandom</backend></rng>
    <memballoon model="virtio"/>
    <serial type="pty"><target type="isa-serial" port="0"/></serial>
  </devices>
</domain>
`, name, def.MemoryMiB, def.MemoryMiB, def.CPUs, def.CPUs, securityModel(def.SecurityModel), architecture(def.Architecture), mac, disk, profileDisk, seed), nil
}

func architecture(value string) string {
	if value == "" {
		return "x86_64"
	}
	return value
}

func (def VMDefinition) ProfileDiskBackendFormat() bool { return def.ProfileDiskFormat }

func securityModel(value string) string {
	switch value {
	case "apparmor", "dac":
		return value
	default:
		return "selinux"
	}
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
	repositoryKeyScript, err := repositoryKeyProvisionScript(def.RepositoryKeyPath)
	if err != nil {
		return "", fmt.Errorf("provision repository private key: %w", err)
	}
	packages, err := provision.PackageManifest(def.Packages)
	if err != nil {
		return "", fmt.Errorf("invalid package manifest: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "arch: %s\nvmType: vz\nplain: true\nrosetta:\n  enabled: false\n  binfmt: false\ncontainerd:\n  system: false\n  user: false\n", architecture(def.Architecture))
	fmt.Fprintf(&b, "cpus: %d\nmemory: %s\ndisk: %dGiB\n", def.CPUs, memorySize(def.MemoryMiB), def.DiskGiB)
	if def.ImagePath != "" {
		fmt.Fprintf(&b, "images:\n  - location: %s\n    arch: %s\n", strconv.Quote(def.ImagePath), architecture(def.Architecture))
	}
	if def.ProfileDiskID != "" {
		fmt.Fprintf(&b, "additionalDisks:\n  - name: %s\n    format: %t\n    fsType: ext4\n", strconv.Quote(def.ProfileDiskID), def.ProfileDiskBackendFormat())
	}
	agentInstructionsScript := provision.AgentInstructionsScript(def.AgentInstructions)
	kindPodmanScript := provision.KindPodmanScript()
	codingAgentsScript := provision.CodingAgentsScript(def.Skills)
	firewallArgs := optionalPort(def.OrcaPort)
	firewallRules, err := FirewallRulesChecked(def.AllowedCIDRs, firewallArgs...)
	if err != nil {
		return "", fmt.Errorf("generate guest firewall: %w", err)
	}
	b.WriteString("mounts: []\nssh:\n  loadDotSSHPubKeys: false\n  forwardAgent: false\n\nprovision:\n  - mode: system\n    script: |")
	b.WriteString("\n      set -eu\n      useradd --create-home --shell /bin/bash agent || true\n")
	b.WriteString("      usermod --lock agent || true\n")
	if def.ProfileDiskID != "" {
		b.WriteString("      install -d -m 0755 /usr/local/libexec\n      cat > /usr/local/libexec/agent-os-profile-sync <<'AGENT_OS_PROFILE_SYNC'\n")
		appendIndented(&b, provision.ProfileSyncScript(), "      ")
		b.WriteString("      AGENT_OS_PROFILE_SYNC\n      chmod 0700 /usr/local/libexec/agent-os-profile-sync\n      cat > /usr/local/libexec/agent-os-profile-setup <<'AGENT_OS_PROFILE_SETUP'\n")
		appendIndented(&b, provision.ProfileSetupScript(provision.ProfileMountSpec{Backend: "lima", DiskID: def.ProfileDiskID, Label: def.ProfileDiskLabel}), "      ")
		b.WriteString("      AGENT_OS_PROFILE_SETUP\n      chmod 0700 /usr/local/libexec/agent-os-profile-setup\n      /bin/bash /usr/local/libexec/agent-os-profile-setup\n")
	}
	b.WriteString("      systemctl disable --now containerd.service 2>/dev/null || true\n")
	if repositoryKeyScript != "" {
		appendIndented(&b, repositoryKeyScript, "      ")
	}
	agentInstructionsDelimiter := heredocDelimiter("AGENT_OS_AGENT_INSTRUCTIONS", agentInstructionsScript)
	fmt.Fprintf(&b, "      cat > /run/agent-os-provision-agent-instructions <<'%s'\n", agentInstructionsDelimiter)
	appendIndented(&b, agentInstructionsScript, "      ")
	fmt.Fprintf(&b, "      %s\n      /bin/bash /run/agent-os-provision-agent-instructions\n", agentInstructionsDelimiter)
	fmt.Fprintf(&b, "      %s\n", strings.Join(provision.InstallCommand(packages), " "))
	b.WriteString("      cat > /run/agent-os-setup-kind-podman <<'AGENT_OS_KIND_PODMAN'\n")
	appendIndented(&b, kindPodmanScript, "      ")
	b.WriteString("      AGENT_OS_KIND_PODMAN\n      /bin/bash /run/agent-os-setup-kind-podman\n")
	b.WriteString("      cat > /run/agent-os-install-coding-agents <<'AGENT_OS_CODING_AGENTS'\n")
	appendIndented(&b, codingAgentsScript, "      ")
	b.WriteString("      AGENT_OS_CODING_AGENTS\n      /bin/bash /run/agent-os-install-coding-agents\n")
	b.WriteString("      cat > /run/agent-os-install-orca <<'AGENT_OS_ORCA_INSTALL'\n")
	appendIndented(&b, orcaInstallScript, "      ")
	b.WriteString("      AGENT_OS_ORCA_INSTALL\n      /bin/bash /run/agent-os-install-orca\n")
	b.WriteString("      install -d -m 0755 /etc/agent-os\n      cat > /etc/systemd/system/orca.service <<'AGENT_OS_ORCA_UNIT'\n")
	appendIndented(&b, OrcaSystemdUnit(def.OrcaPort, def.BindAddress, def.PairingAddress), "      ")
	b.WriteString("      AGENT_OS_ORCA_UNIT\n")
	b.WriteString("      cat > /etc/agent-os/firewall.rules <<'AGENT_OS_FIREWALL'\n")
	appendIndented(&b, strings.Join(firewallRules, "\n"), "      ")
	b.WriteString("      AGENT_OS_FIREWALL\n      cat > /etc/systemd/system/agent-os-firewall.service <<'AGENT_OS_FIREWALL_UNIT'\n")
	appendIndented(&b, FirewallSystemdUnit(), "      ")
	b.WriteString("      AGENT_OS_FIREWALL_UNIT\n      systemctl daemon-reload\n      systemctl enable --now agent-os-firewall.service\n      systemctl enable --now orca.service\n")
	return b.String(), nil
}

func CloudInit(def VMDefinition, repositoryKeyPath string) string {
	contents, err := cloudInit(def, repositoryKeyPath)
	if err == nil {
		return contents
	}

	// The CLI validates the key before a normal create. Keep this legacy
	// string-returning API safe for callers that bypass that validation: emit a
	// usable artifact without the key, but never include the host path or the
	// validation error in guest data. Callers that need an error can use
	// CloudInitWithError.
	fallbackDefinition := def
	fallbackDefinition.RepositoryKeyPath = ""
	fallback, fallbackErr := cloudInit(fallbackDefinition, "")
	if fallbackErr == nil {
		return strings.Replace(fallback, "#cloud-config\n", "#cloud-config\n# repository private key was not provisioned\n", 1)
	}
	return "#cloud-config\n# repository private key was not provisioned\n"
}

// CloudInitWithError is the error-returning form used by tests and callers
// that want to fail creation rather than receive the compatibility fallback
// from CloudInit.
func CloudInitWithError(def VMDefinition, repositoryKeyPath string) (string, error) {
	return cloudInit(def, repositoryKeyPath)
}

func cloudInit(def VMDefinition, repositoryKeyPath string) (string, error) {
	if strings.TrimSpace(repositoryKeyPath) == "" {
		repositoryKeyPath = def.RepositoryKeyPath
	}
	guestKeyPath, encodedKey, err := repositoryKeyData(repositoryKeyPath)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	b.WriteString("users:\n  - name: agent\n    gecos: Agent\n    groups: []\n    shell: /bin/bash\n    lock_passwd: true\n    sudo: []\n")
	b.WriteString("ssh_pwauth: false\npackages:\n")
	packages, err := provision.PackageManifest(def.Packages)
	if err != nil {
		return "#cloud-config\n# invalid package manifest: " + err.Error() + "\n", nil
	}
	firewallArgs := optionalPort(def.OrcaPort)
	firewallRules, err := FirewallRulesChecked(def.AllowedCIDRs, firewallArgs...)
	if err != nil {
		return "", fmt.Errorf("generate guest firewall: %w", err)
	}
	if !containsString(packages, "qemu-guest-agent") {
		packages = append(packages, "qemu-guest-agent")
	}
	sort.Strings(packages)
	for _, pkg := range packages {
		fmt.Fprintf(&b, "  - %s\n", pkg)
	}
	if encodedKey != "" {
		b.WriteString("bootcmd:\n  - [install, -d, -o, root, -g, root, -m, '0700', /etc/agent-os/keys]\n")
	}
	b.WriteString("write_files:\n")
	b.WriteString("  - path: /etc/agent-os/README\n    permissions: '0644'\n    content: |\n      Managed by agent-os. Repository credentials use restricted guest permissions.\n")
	if encodedKey != "" {
		fmt.Fprintf(&b, "  - path: %s\n    owner: root:root\n    permissions: '0600'\n    defer: true\n    encoding: b64\n    content: |\n      %s\n", strconv.Quote(guestKeyPath), encodedKey)
	}
	b.WriteString("  - path: /etc/systemd/system/orca.service\n    permissions: '0644'\n    content: |\n")
	appendIndented(&b, OrcaSystemdUnit(def.OrcaPort, def.BindAddress, def.PairingAddress), "      ")
	b.WriteString("  - path: /usr/local/libexec/agent-os-provision-agent-instructions\n    permissions: '0700'\n    content: |\n")
	appendIndented(&b, provision.AgentInstructionsScript(def.AgentInstructions), "      ")
	if def.ProfileDiskID != "" {
		b.WriteString("  - path: /usr/local/libexec/agent-os-profile-sync\n    permissions: '0700'\n    content: |\n")
		appendIndented(&b, provision.ProfileSyncScript(), "      ")
		b.WriteString("  - path: /usr/local/libexec/agent-os-profile-setup\n    permissions: '0700'\n    content: |\n")
		appendIndented(&b, provision.ProfileSetupScript(provision.ProfileMountSpec{Backend: "libvirt", DiskID: def.ProfileDiskID, Label: def.ProfileDiskLabel}), "      ")
	}
	b.WriteString("  - path: /usr/local/libexec/agent-os-setup-kind-podman\n    permissions: '0700'\n    content: |\n")
	appendIndented(&b, provision.KindPodmanScript(), "      ")
	b.WriteString("  - path: /usr/local/libexec/agent-os-install-coding-agents\n    permissions: '0700'\n    content: |\n")
	appendIndented(&b, provision.CodingAgentsScript(def.Skills), "      ")
	b.WriteString("  - path: /etc/agent-os/firewall.rules\n    permissions: '0600'\n    content: |\n")
	appendIndented(&b, strings.Join(firewallRules, "\n"), "      ")
	orcaInstallScript, err := releases.OrcaInstallScript(def.Architecture)
	if err != nil {
		return "", err
	}
	b.WriteString("  - path: /usr/local/libexec/agent-os-install-orca\n    permissions: '0700'\n    content: |\n")
	appendIndented(&b, orcaInstallScript, "      ")
	b.WriteString("  - path: /etc/systemd/system/agent-os-firewall.service\n    permissions: '0644'\n    content: |\n")
	appendIndented(&b, FirewallSystemdUnit(), "      ")
	b.WriteString("runcmd:\n")
	if def.ProfileDiskID != "" {
		b.WriteString("  - [bash, /usr/local/libexec/agent-os-profile-setup]\n")
	}
	b.WriteString("  - [bash, /usr/local/libexec/agent-os-provision-agent-instructions]\n")
	b.WriteString("  - [bash, /usr/local/libexec/agent-os-setup-kind-podman]\n")
	b.WriteString("  - [bash, /usr/local/libexec/agent-os-install-coding-agents]\n")
	b.WriteString("  - [bash, /usr/local/libexec/agent-os-install-orca]\n")
	b.WriteString("  - [systemctl, daemon-reload]\n")
	b.WriteString("  - [systemctl, enable, --now, agent-os-firewall.service]\n")
	b.WriteString("  - [systemctl, enable, --now, qemu-guest-agent.service]\n")
	b.WriteString("  - [systemctl, enable, --now, orca.service]\n")
	return b.String(), nil
}

func repositoryKeyData(repositoryKeyPath string) (guestPath, encoded string, err error) {
	if strings.TrimSpace(repositoryKeyPath) == "" {
		return "", "", nil
	}
	data, err := credentials.ReadPrivateKey(repositoryKeyPath)
	if err != nil {
		return "", "", err
	}
	return credentials.GuestKeyPath(repositoryKeyPath), base64.StdEncoding.EncodeToString(data), nil
}

func repositoryKeyProvisionScript(repositoryKeyPath string) (string, error) {
	guestPath, encoded, err := repositoryKeyData(repositoryKeyPath)
	if err != nil {
		return "", err
	}
	if encoded == "" {
		return "", nil
	}

	const encodedPath = "/run/agent-os-repository-key.b64"
	var b strings.Builder
	b.WriteString("umask 077\n")
	b.WriteString("install -d -o root -g root -m 0700 /etc/agent-os/keys\n")
	fmt.Fprintf(&b, "cat > %s <<'AGENT_OS_REPOSITORY_KEY'\n", shellQuote(encodedPath))
	b.WriteString(encoded + "\nAGENT_OS_REPOSITORY_KEY\n")
	fmt.Fprintf(&b, "/usr/bin/base64 --decode %s > %s\n", shellQuote(encodedPath), shellQuote(guestPath))
	fmt.Fprintf(&b, "chown root:root %s\nchmod 0600 %s\n", shellQuote(guestPath), shellQuote(guestPath))
	fmt.Fprintf(&b, "rm -f -- %s\n", shellQuote(encodedPath))
	return strings.TrimSuffix(b.String(), "\n"), nil
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func optionalPort(port int) []int {
	if port == 0 {
		return nil
	}
	return []int{port}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func appendIndented(b *strings.Builder, value, prefix string) {
	for _, line := range strings.Split(strings.TrimSuffix(value, "\n"), "\n") {
		b.WriteString(prefix + line + "\n")
	}
}

func heredocDelimiter(prefix, value string) string {
	delimiter := prefix
	for strings.Contains(value, delimiter) {
		delimiter += "_"
	}
	return delimiter
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
	// Preserve the historical best-effort API: an omitted or out-of-range
	// optional port simply omits the ingress rule. Artifact generators use the
	// checked form below, so invalid configuration still fails creation.
	if len(orcaPort) > 1 {
		orcaPort = orcaPort[:1]
	}
	if len(orcaPort) == 1 && (orcaPort[0] < 1 || orcaPort[0] > 65535) {
		orcaPort = nil
	}
	rules, err := firewallRules(allowedCIDRs, orcaPort, false)
	if err != nil {
		return nil
	}
	return rules
}

// FirewallRulesChecked returns an nftables ruleset in the native script
// format accepted by `nft -f`. It rejects invalid ports or CIDRs so callers
// that are creating guest artifacts cannot accidentally interpolate arbitrary
// text into a firewall rule.
func FirewallRulesChecked(allowedCIDRs []string, orcaPort ...int) ([]string, error) {
	return firewallRules(allowedCIDRs, orcaPort, true)
}

func firewallRules(allowedCIDRs []string, orcaPort []int, rejectInvalidCIDR bool) ([]string, error) {
	if len(orcaPort) > 1 {
		return nil, fmt.Errorf("Orca port may be specified at most once")
	}
	port := 0
	if len(orcaPort) == 1 {
		port = orcaPort[0]
		if port < 1 || port > 65535 {
			return nil, fmt.Errorf("invalid Orca port %d", port)
		}
	}

	cidrs := make([]string, 0, len(allowedCIDRs))
	seen := make(map[string]struct{}, len(allowedCIDRs))
	for _, raw := range allowedCIDRs {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			if rejectInvalidCIDR {
				return nil, fmt.Errorf("invalid allowed CIDR %q", value)
			}
			continue
		}
		value = network.String()
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		cidrs = append(cidrs, value)
	}
	sort.Strings(cidrs)

	rules := []string{
		"add table inet agent_os",
		"add chain inet agent_os forward { type filter hook forward priority 0; policy drop; }",
		"add rule inet agent_os forward ct state established,related accept",
		"add chain inet agent_os output { type filter hook output priority 0; policy drop; }",
		"add rule inet agent_os output ct state established,related accept",
		"add rule inet agent_os output oifname != \"lo\" udp dport { 53, 67, 68, 123 } accept",
		"add rule inet agent_os output oifname != \"lo\" tcp dport 53 accept",
		"add rule inet agent_os output oifname != \"lo\" ip daddr != { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 169.254.0.0/16 } accept",
		"add rule inet agent_os output oifname != \"lo\" ip6 daddr != { ::1/128, fc00::/7, fe80::/10 } accept",
		"add rule inet agent_os output oifname != \"lo\" ip6 daddr fc00::/7 drop",
		"add rule inet agent_os output oifname \"lo\" accept",
		"add chain inet agent_os input { type filter hook input priority 0; policy drop; }",
		"add rule inet agent_os input ct state established,related accept",
		"add rule inet agent_os input iifname \"lo\" accept",
		"add rule inet agent_os input iifname != \"lo\" udp sport 67 udp dport 68 accept",
	}
	if port != 0 {
		rules = append(rules, fmt.Sprintf("add rule inet agent_os input iifname != \"lo\" tcp dport %d accept", port))
	}
	for _, cidr := range cidrs {
		addressFamily := "ip"
		if strings.Contains(cidr, ":") {
			addressFamily = "ip6"
		}
		rules = append(rules, fmt.Sprintf("add rule inet agent_os output oifname != \"lo\" %s daddr %s accept", addressFamily, cidr))
	}
	return rules, nil
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
ExecStartPre=-/usr/sbin/nft delete table inet agent_os
ExecStart=/usr/sbin/nft -f /etc/agent-os/firewall.rules
RemainAfterExit=yes
ExecReload=-/usr/sbin/nft delete table inet agent_os
ExecReload=/usr/sbin/nft -f /etc/agent-os/firewall.rules

[Install]
WantedBy=multi-user.target
`
}

func PackageManifest(packages []string) []string {
	result, err := provision.PackageManifest(packages)
	if err != nil {
		return nil
	}
	return result
}
