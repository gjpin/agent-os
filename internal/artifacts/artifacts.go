package artifacts

import (
	"encoding/base64"
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
	VMType            string
	AccessMode        model.AccessMode
	OrcaPort          int
	Packages          []string
	Skills            []string
	ImagePath         string
	BindAddress       string
	PairingAddress    string
	AllowedCIDRs      []string
	AgentInstructions string
	RepositoryKeyPath string
	ProfileDiskID     string
	ProfileDiskLabel  string
	ProfileDiskFormat bool
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
		AllowedCIDRs:      append([]string(nil), c.AllowedCIDRs...),
		RepositoryKeyPath: c.RepositoryKeyPath,
	}
}

func architecture(value string) string {
	if value == "" {
		return "x86_64"
	}
	return value
}

func memorySize(mib int) string {
	if mib%1024 == 0 {
		return fmt.Sprintf("%dGiB", mib/1024)
	}
	return fmt.Sprintf("%dMiB", mib)
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
	if def.VMType != "qemu" && def.VMType != "vz" {
		return "", fmt.Errorf("unsupported Lima VM type %q", def.VMType)
	}
	fmt.Fprintf(&b, "arch: %s\nvmType: %s\nplain: true\nrosetta:\n  enabled: false\n  binfmt: false\ncontainerd:\n  system: false\n  user: false\n", architecture(def.Architecture), def.VMType)
	fmt.Fprintf(&b, "cpus: %d\nmemory: %s\ndisk: %dGiB\n", def.CPUs, memorySize(def.MemoryMiB), def.DiskGiB)
	if def.ImagePath != "" {
		fmt.Fprintf(&b, "images:\n  - location: %s\n    arch: %s\n", strconv.Quote(def.ImagePath), architecture(def.Architecture))
	}
	if def.ProfileDiskID != "" {
		fmt.Fprintf(&b, "additionalDisks:\n  - name: %s\n    format: %t\n    fsType: ext4\n", strconv.Quote(def.ProfileDiskID), def.ProfileDiskFormat)
	}
	agentInstructionsScript := provision.AgentInstructionsScript(def.AgentInstructions)
	kindPodmanScript := provision.KindPodmanScript()
	codingAgentsScript := provision.CodingAgentsScript(def.Skills)
	orcaSkillsScript := provision.OrcaSkillsScript()
	firewallArgs := optionalPort(def.OrcaPort)
	firewallRules, err := FirewallRulesChecked(def.AllowedCIDRs, firewallArgs...)
	if err != nil {
		return "", fmt.Errorf("generate guest firewall: %w", err)
	}
	var provisioning strings.Builder
	provisioning.WriteString("#!/bin/bash\nset -eu\nuseradd --create-home --shell /bin/bash agent || true\n")
	provisioning.WriteString("usermod --lock agent || true\n")
	if def.ProfileDiskID != "" {
		provisioning.WriteString("install -d -m 0755 /usr/local/libexec\ncat > /usr/local/libexec/agent-os-profile-sync <<'AGENT_OS_PROFILE_SYNC'\n")
		appendIndented(&provisioning, provision.ProfileSyncScript(), "")
		provisioning.WriteString("AGENT_OS_PROFILE_SYNC\nchmod 0700 /usr/local/libexec/agent-os-profile-sync\ncat > /usr/local/libexec/agent-os-profile-setup <<'AGENT_OS_PROFILE_SETUP'\n")
		appendIndented(&provisioning, provision.ProfileSetupScript(provision.ProfileMountSpec{DiskID: def.ProfileDiskID, Label: def.ProfileDiskLabel}), "")
		provisioning.WriteString("AGENT_OS_PROFILE_SETUP\nchmod 0700 /usr/local/libexec/agent-os-profile-setup\n/bin/bash /usr/local/libexec/agent-os-profile-setup\n")
	}
	provisioning.WriteString("systemctl disable --now containerd.service 2>/dev/null || true\n")
	if repositoryKeyScript != "" {
		appendIndented(&provisioning, repositoryKeyScript, "")
	}
	agentInstructionsDelimiter := heredocDelimiter("AGENT_OS_AGENT_INSTRUCTIONS", agentInstructionsScript)
	fmt.Fprintf(&provisioning, "cat > /run/agent-os-provision-agent-instructions <<'%s'\n", agentInstructionsDelimiter)
	appendIndented(&provisioning, agentInstructionsScript, "")
	fmt.Fprintf(&provisioning, "%s\n/bin/bash /run/agent-os-provision-agent-instructions\n", agentInstructionsDelimiter)
	fmt.Fprintf(&provisioning, "%s\n", strings.Join(provision.InstallCommand(packages), " "))
	provisioning.WriteString("cat > /run/agent-os-setup-kind-podman <<'AGENT_OS_KIND_PODMAN'\n")
	appendIndented(&provisioning, kindPodmanScript, "")
	provisioning.WriteString("AGENT_OS_KIND_PODMAN\n/bin/bash /run/agent-os-setup-kind-podman\n")
	provisioning.WriteString("cat > /run/agent-os-install-coding-agents <<'AGENT_OS_CODING_AGENTS'\n")
	appendIndented(&provisioning, codingAgentsScript, "")
	provisioning.WriteString("AGENT_OS_CODING_AGENTS\n/bin/bash /run/agent-os-install-coding-agents\n")
	if def.ProfileDiskID != "" {
		provisioning.WriteString("/usr/local/libexec/agent-os-profile-sync sync\n")
	}
	provisioning.WriteString("cat > /run/agent-os-install-orca <<'AGENT_OS_ORCA_INSTALL'\n")
	appendIndented(&provisioning, orcaInstallScript, "")
	provisioning.WriteString("AGENT_OS_ORCA_INSTALL\n/bin/bash /run/agent-os-install-orca\n")
	provisioning.WriteString("cat > /run/agent-os-install-orca-skills <<'AGENT_OS_ORCA_SKILLS'\n")
	appendIndented(&provisioning, orcaSkillsScript, "")
	provisioning.WriteString("AGENT_OS_ORCA_SKILLS\n/bin/bash /run/agent-os-install-orca-skills\n")
	provisioning.WriteString("install -d -m 0755 /etc/agent-os\ncat > /etc/systemd/system/orca.service <<'AGENT_OS_ORCA_UNIT'\n")
	orcaUnit := OrcaSystemdUnit(def.OrcaPort, def.BindAddress, def.PairingAddress)
	if def.ProfileDiskID != "" {
		orcaUnit = OrcaSystemdUnitWithProfile(def.OrcaPort, def.BindAddress, def.PairingAddress)
	}
	appendIndented(&provisioning, orcaUnit, "")
	provisioning.WriteString("AGENT_OS_ORCA_UNIT\n")
	if def.ProfileDiskID != "" {
		provisioning.WriteString("cat > /etc/systemd/system/agent-os-profile-restore.service <<'AGENT_OS_PROFILE_RESTORE_UNIT'\n")
		appendIndented(&provisioning, provision.ProfileRestoreSystemdUnit(), "")
		provisioning.WriteString("AGENT_OS_PROFILE_RESTORE_UNIT\n")
	}
	provisioning.WriteString("cat > /etc/agent-os/firewall.rules <<'AGENT_OS_FIREWALL'\n")
	appendIndented(&provisioning, strings.Join(firewallRules, "\n"), "")
	provisioning.WriteString("AGENT_OS_FIREWALL\ncat > /etc/systemd/system/agent-os-firewall.service <<'AGENT_OS_FIREWALL_UNIT'\n")
	appendIndented(&provisioning, FirewallSystemdUnit(), "")
	provisioning.WriteString("AGENT_OS_FIREWALL_UNIT\nsystemctl daemon-reload\n")
	if def.ProfileDiskID != "" {
		provisioning.WriteString("systemctl enable --now agent-os-profile-restore.service\n")
	}
	provisioning.WriteString("systemctl enable --now agent-os-firewall.service\nsystemctl enable --now orca.service\n")
	if def.OrcaPort != 0 {
		provisioning.WriteString("cat > /usr/local/libexec/agent-os-wait-for-orca <<'AGENT_OS_WAIT_FOR_ORCA'\n")
		appendIndented(&provisioning, orcaReadinessScript(def.OrcaPort), "")
		provisioning.WriteString("AGENT_OS_WAIT_FOR_ORCA\nchmod 0700 /usr/local/libexec/agent-os-wait-for-orca\n/bin/bash /usr/local/libexec/agent-os-wait-for-orca\n")
	}
	fmt.Fprintf(&provisioning, "install -d -m 0755 /var/lib/agent-os\ntouch %s\nchmod 0644 %s\n", provision.ProvisioningReadyPath, provision.ProvisioningReadyPath)

	b.WriteString("mounts: []\nssh:\n  loadDotSSHPubKeys: false\n  forwardAgent: false\n\nprovision:\n  - mode: system\n    script: |\n      install -d -m 0755 /usr/local/libexec\n      cat > /usr/local/libexec/agent-os-provision <<'AGENT_OS_PROVISION'\n")
	appendIndented(&b, provisioning.String(), "      ")
	b.WriteString("      AGENT_OS_PROVISION\n      chmod 0700 /usr/local/libexec/agent-os-provision\n      cat > /etc/systemd/system/agent-os-provision.service <<'AGENT_OS_PROVISION_UNIT'\n")
	appendIndented(&b, limaProvisioningUnit(), "      ")
	b.WriteString("      AGENT_OS_PROVISION_UNIT\n      systemctl daemon-reload\n      systemctl enable agent-os-provision.service\n      systemctl start --no-block agent-os-provision.service\n")
	return b.String(), nil
}

func limaProvisioningUnit() string {
	return `[Unit]
Description=agent-os guest provisioning
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/bin/bash /usr/local/libexec/agent-os-provision
TimeoutStartSec=infinity
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
`
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
Environment=HOME=/home/agent
Environment=ORCA_MODE=headless
Environment=ORCA_PORT=%d
Environment=ORCA_BIND_ADDRESS=%s
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=false
ReadWritePaths=/home/agent /var/lib/agent-os/profile

[Install]
WantedBy=multi-user.target
`, port, pairing, port, bindAddress)
}

func orcaReadinessScript(port int) string {
	return fmt.Sprintf(`#!/bin/bash
set -euo pipefail

wait_for_configured_port() {
  for _ in $(seq 1 60); do
    if ss -H -ltn | awk -v port=":%d" '$4 ~ (port "$") { found=1 } END { exit !found }'; then
      return 0
    fi
    if ! systemctl is-active --quiet orca.service; then
      return 1
    fi
    sleep 1
  done
  return 1
}

if wait_for_configured_port; then
  exit 0
fi

# Orca can fall back to an OS-assigned port when a stale process wins the
# initial bind race. Restart once so the configured endpoint is authoritative.
systemctl restart orca.service
if wait_for_configured_port; then
  exit 0
fi

echo "Orca did not listen on configured TCP port %d" >&2
systemctl status --no-pager orca.service >&2 || true
ss -H -ltn >&2 || true
exit 1
`, port, port)
}

// OrcaSystemdUnitWithProfile makes the profile restore a hard dependency of
// Orca. If the profile cannot be mounted or restored, Orca must not start on
// an empty guest home and risk writing state outside the retained disk.
func OrcaSystemdUnitWithProfile(port int, bindAddress string, pairingAddress ...string) string {
	unit := OrcaSystemdUnit(port, bindAddress, pairingAddress...)
	return strings.Replace(unit,
		"After=network-online.target\nWants=network-online.target",
		"After=network-online.target agent-os-profile-restore.service\nRequires=agent-os-profile-restore.service\nWants=network-online.target",
		1,
	)
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

// FirewallRulesChecked returns an nftables ruleset in the native script
// format accepted by `nft -f`. It rejects invalid ports or CIDRs so callers
// that are creating guest artifacts cannot accidentally interpolate arbitrary
// text into a firewall rule.

func FirewallRulesChecked(allowedCIDRs []string, orcaPort ...int) ([]string, error) {
	return firewallRules(allowedCIDRs, orcaPort)
}

func firewallRules(allowedCIDRs []string, orcaPort []int) ([]string, error) {
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
			return nil, fmt.Errorf("invalid allowed CIDR %q", value)
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
		"add rule inet agent_os input iifname != \"lo\" tcp dport 22 accept",
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
