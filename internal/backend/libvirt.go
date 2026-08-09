package backend

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gjpin/agent-os/internal/artifacts"
	"github.com/gjpin/agent-os/internal/execx"
	"github.com/gjpin/agent-os/internal/logging"
	"github.com/gjpin/agent-os/internal/model"
	"github.com/gjpin/agent-os/internal/provision"
	"github.com/gjpin/agent-os/internal/releases"
)

type Libvirt struct {
	Runner execx.Runner
	Out    io.Writer
	Err    io.Writer
}

const (
	qemuGuestAgentPackage        = "qemu-guest-agent"
	qemuGuestAgentCommandTimeout = "30"
)

var guestAgentPollInterval = 100 * time.Millisecond

var (
	provisioningTimeout      = 15 * time.Minute
	provisioningPollInterval = time.Second
)

func (l Libvirt) Name() string { return "libvirt" }

func (l Libvirt) Available(ctx context.Context) error {
	if err := command(l.Runner, ctx, "virsh", []string{"--connect", "qemu:///system", "uri"}, nil, l.Out, l.Err); err != nil {
		return fmt.Errorf("libvirt is unavailable: %w", err)
	}
	if err := libvirtSecurityPreflight(); err != nil {
		return err
	}
	return nil
}

func (l Libvirt) Create(ctx context.Context, spec Spec) error {
	if err := spec.Config.Validate(); err != nil {
		return fmt.Errorf("invalid libvirt configuration: %w", err)
	}
	if _, err := libvirtForwardingRules(spec); err != nil {
		return err
	}
	if !spec.DryRun {
		if err := libvirtSecurityPreflight(); err != nil {
			return err
		}
	}
	definition := libvirtDefinition(spec.Config, spec.Architecture)
	definition.SecurityModel = libvirtSecurityModel()
	definition.AgentInstructions = spec.AgentInstructions
	artifactDir := filepath.Join(spec.Config.StateDir, "v1", "vms", spec.Config.VMName, "artifacts")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		return err
	}
	disk := filepath.Join(artifactDir, "disk.qcow2")
	seed := filepath.Join(artifactDir, "cloud-init.iso")
	xmlPath := filepath.Join(artifactDir, "domain.xml")
	xml, err := artifacts.LibvirtXML(definition, disk, seed)
	if err != nil {
		return err
	}
	if err := os.WriteFile(xmlPath, []byte(xml), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "network.xml"), []byte(artifacts.LibvirtNetworkXML(definition)), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "orca.service"), []byte(artifacts.OrcaSystemdUnit(definition.OrcaPort, definition.BindAddress, definition.PairingAddress)), 0o600); err != nil {
		return err
	}
	userData, err := artifacts.CloudInitWithError(definition, spec.Config.RepositoryKeyPath)
	if err != nil {
		return fmt.Errorf("generate cloud-init: %w", err)
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "user-data"), []byte(userData), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "meta-data"), []byte("instance-id: "+spec.Config.VMName+"\nlocal-hostname: "+spec.Config.VMName+"\n"), 0o600); err != nil {
		return err
	}
	if spec.DryRun {
		return nil
	}
	image, err := releases.FedoraServer44(spec.Architecture)
	if err != nil {
		return err
	}
	base := filepath.Join(artifactDir, image.Filename)
	if _, err := os.Stat(base); err != nil {
		if err := releases.DownloadVerified(ctx, l.Runner, image, base, l.Out, l.Err); err != nil {
			return err
		}
	}
	if err := l.ensureNetwork(ctx, filepath.Join(artifactDir, "network.xml")); err != nil {
		return err
	}
	if err := l.verifyLibvirtSecurityTranslation(ctx, xmlPath); err != nil {
		return err
	}
	// All arguments are fixed values or validated paths. No shell is involved.
	if err := command(l.Runner, ctx, "qemu-img", []string{"create", "-f", "qcow2", "-F", "qcow2", "-b", base, "-o", "size=" + fmt.Sprintf("%dG", definition.DiskGiB), disk}, nil, l.Out, l.Err); err != nil {
		return err
	}
	if err := command(l.Runner, ctx, "cloud-localds", []string{seed, filepath.Join(artifactDir, "user-data"), filepath.Join(artifactDir, "meta-data")}, nil, l.Out, l.Err); err != nil {
		return err
	}
	return command(l.Runner, ctx, "virsh", []string{"--connect", "qemu:///system", "define", xmlPath}, nil, l.Out, l.Err)
}

func libvirtDefinition(c model.Config, architecture string) artifacts.VMDefinition {
	definition := artifacts.FromConfig(c, architecture)
	// The WireGuard address belongs to the host. The stable DHCP reservation
	// belongs to this guest, so bind Orca to that guest address and let the
	// host forwarding rules select the external endpoint.
	definition.BindAddress = definition.GuestAddress
	if !containsPackage(definition.Packages, qemuGuestAgentPackage) {
		definition.Packages = append(definition.Packages, qemuGuestAgentPackage)
	}
	sort.Strings(definition.Packages)
	return definition
}

func containsPackage(packages []string, wanted string) bool {
	for _, pkg := range packages {
		if pkg == wanted {
			return true
		}
	}
	return false
}

func (l Libvirt) EnsureNetwork(ctx context.Context, spec Spec) error {
	artifactDir := filepath.Join(spec.Config.StateDir, "v1", "vms", spec.Config.VMName, "artifacts")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		return err
	}
	definition := libvirtDefinition(spec.Config, spec.Architecture)
	path := filepath.Join(artifactDir, "network.xml")
	if err := os.WriteFile(path, []byte(artifacts.LibvirtNetworkXML(definition)), 0o600); err != nil {
		return err
	}
	return l.ensureNetwork(ctx, path)
}

// ConfigureForwarding installs a per-VM nftables DNAT rule on the Linux host.
// The libvirt network itself only provides outbound NAT; it does not expose a
// guest service on a selected host address.
func (l Libvirt) ConfigureForwarding(ctx context.Context, spec Spec) error {
	rules, err := libvirtForwardingRules(spec)
	if err != nil {
		return err
	}
	if spec.Config.AccessMode == model.AccessWireGuard {
		if err := command(l.Runner, ctx, "sudo", []string{"ip", "link", "show", "dev", spec.Config.WireGuardInterface}, nil, l.Out, l.Err); err != nil {
			return fmt.Errorf("WireGuard interface %q is unavailable: %w", spec.Config.WireGuardInterface, err)
		}
	}
	// Reapplying start is idempotent. A stale table can remain after an
	// interrupted lifecycle operation, so remove only this VM's table first.
	// Do not ignore a real cleanup failure: installing a new ruleset after a
	// failed delete can leave an ambiguous or partially applied policy.
	if err := l.deleteForwardingTable(ctx, spec); err != nil {
		return err
	}
	if err := command(l.Runner, ctx, "sudo", []string{"sysctl", "-w", "net.ipv4.ip_forward=1"}, nil, l.Out, l.Err); err != nil {
		return fmt.Errorf("enable IPv4 forwarding: %w", err)
	}
	if err := command(l.Runner, ctx, "sudo", []string{"nft", "-f", "-"}, strings.NewReader(rules), l.Out, l.Err); err != nil {
		return fmt.Errorf("install Orca forwarding rules: %w", err)
	}
	return nil
}

func libvirtForwardingRules(spec Spec) (string, error) {
	if err := validateLibvirtVMName(spec.Config.VMName); err != nil {
		return "", err
	}
	if !spec.Config.AccessMode.Valid() {
		return "", fmt.Errorf("invalid access mode %q", spec.Config.AccessMode)
	}
	if spec.Config.AccessMode == model.AccessWireGuard {
		if err := validateWireGuardAddress(spec.Config.WireGuardAddress); err != nil {
			return "", err
		}
	}
	rules, err := artifacts.LinuxForwardingRules(spec.Config, artifacts.LibvirtGuestAddress(spec.Config.VMName))
	if err != nil {
		return "", fmt.Errorf("invalid libvirt forwarding configuration: %w", err)
	}
	return rules, nil
}

func validateWireGuardAddress(value string) error {
	if ip := net.ParseIP(value); ip != nil {
		if ip.To4() == nil {
			return fmt.Errorf("WireGuard forwarding requires an IPv4 address")
		}
		return nil
	}
	_, network, err := net.ParseCIDR(value)
	if err != nil || network.IP.To4() == nil {
		return fmt.Errorf("WireGuard forwarding requires an IPv4 address or CIDR")
	}
	return nil
}

func validateLibvirtVMName(name string) error {
	if !model.VMNameIsValid(name) {
		return fmt.Errorf("invalid VM name %q", name)
	}
	return nil
}

func (l Libvirt) RemoveForwarding(ctx context.Context, spec Spec) error {
	if err := validateLibvirtVMName(spec.Config.VMName); err != nil {
		return err
	}
	return l.deleteForwardingTable(ctx, spec)
}

func (l Libvirt) deleteForwardingTable(ctx context.Context, spec Spec) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateLibvirtVMName(spec.Config.VMName); err != nil {
		return err
	}
	// nft reports a missing table as an error. Treat that case as success so
	// stop/destroy remain safe when forwarding was never installed.
	var listing bytes.Buffer
	if err := command(l.Runner, ctx, "sudo", []string{"nft", "list", "table", "ip", artifacts.ForwardingTableName(spec.Config.VMName)}, nil, &listing, io.Discard); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return nil
	}
	if err := command(l.Runner, ctx, "sudo", []string{"nft", "delete", "table", "ip", artifacts.ForwardingTableName(spec.Config.VMName)}, nil, l.Out, l.Err); err != nil {
		return fmt.Errorf("remove Orca forwarding rules: %w", err)
	}
	return nil
}

func (l Libvirt) ensureNetwork(ctx context.Context, definitionPath string) error {
	var info bytes.Buffer
	infoErr := command(l.Runner, ctx, "virsh", []string{"--connect", "qemu:///system", "net-info", "agent-os-nat"}, nil, &info, l.Err)
	if infoErr != nil {
		if err := command(l.Runner, ctx, "virsh", []string{"--connect", "qemu:///system", "net-define", definitionPath}, nil, l.Out, l.Err); err != nil {
			return err
		}
	}
	active := strings.ToLower(info.String())
	active = strings.ReplaceAll(active, " ", "")
	if infoErr != nil || !strings.Contains(active, "active:yes") {
		if err := command(l.Runner, ctx, "virsh", []string{"--connect", "qemu:///system", "net-start", "agent-os-nat"}, nil, l.Out, l.Err); err != nil {
			return err
		}
	}
	return command(l.Runner, ctx, "virsh", []string{"--connect", "qemu:///system", "net-autostart", "agent-os-nat"}, nil, l.Out, l.Err)
}

func libvirtSecurityModel() string {
	return libvirtSecurityModelAt("/etc/os-release")
}

func libvirtSecurityModelAt(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "selinux"
	}
	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(rawLine)
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "ID" {
			continue
		}
		distribution := strings.ToLower(strings.Trim(strings.TrimSpace(value), "\"'"))
		switch distribution {
		case "arch":
			return "dac"
		case "ubuntu":
			return "apparmor"
		case "fedora":
			return "selinux"
		}
	}
	return "selinux"
}

// libvirtSecurityPreflight checks the host-wide controls that libvirt applies
// to every QEMU process. They are not valid per-domain XML knobs: a custom
// metadata namespace would only annotate the domain and a duplicate QEMU
// -sandbox argument can conflict with libvirt's own command line. The system
// driver must have an explicit qemu.conf policy so creation never depends on
// an implicit distribution default or an unverified host configuration.
func libvirtSecurityPreflight() error {
	return libvirtSecurityPreflightAt("/etc/libvirt/qemu.conf")
}

func libvirtSecurityPreflightAt(configPath string) error {
	data, err := os.ReadFile(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("libvirt security policy is not explicit: %s is missing; set seccomp_sandbox = 1, namespaces = [ \"mount\" ], and include \"devices\" in cgroup_controllers", configPath)
	}
	if err != nil {
		return fmt.Errorf("read libvirt security configuration %s: %w", configPath, err)
	}
	return validateLibvirtSecurityConfig(data)
}

func validateLibvirtSecurityConfig(data []byte) error {
	settings, err := parseLibvirtQEMUConfig(data)
	if err != nil {
		return fmt.Errorf("parse libvirt security configuration: %w", err)
	}

	seccomp, ok := settings["seccomp_sandbox"]
	if !ok || strings.TrimSpace(seccomp) != "1" {
		return errors.New("libvirt seccomp sandbox must be explicitly enabled with seccomp_sandbox = 1 in /etc/libvirt/qemu.conf")
	}

	namespaces, err := libvirtStringList(settings, "namespaces")
	if err != nil {
		return err
	}
	if !containsFolded(namespaces, "mount") {
		return errors.New("libvirt private mount namespace must be explicitly enabled in /etc/libvirt/qemu.conf")
	}

	controllers, err := libvirtStringList(settings, "cgroup_controllers")
	if err != nil {
		return err
	}
	if !containsFolded(controllers, "devices") {
		return errors.New("libvirt device cgroup control must be explicitly enabled in /etc/libvirt/qemu.conf")
	}

	if value, ok := settings["security_default_confined"]; ok && libvirtValueDisabled(value) {
		return errors.New("libvirt security confinement is explicitly disabled in /etc/libvirt/qemu.conf")
	}
	if value, ok := settings["security_driver"]; ok {
		trimmed := strings.TrimSpace(value)
		if strings.HasPrefix(trimmed, "[") {
			drivers, err := libvirtStringList(settings, "security_driver")
			if err != nil {
				return err
			}
			if len(drivers) == 0 || containsFolded(drivers, "none") {
				return errors.New("libvirt security drivers are explicitly disabled in /etc/libvirt/qemu.conf")
			}
		} else if strings.EqualFold(strings.Trim(trimmed, "\"'"), "none") {
			return errors.New("libvirt security drivers are explicitly disabled in /etc/libvirt/qemu.conf")
		}
	}
	return nil
}

func libvirtValueDisabled(value string) bool {
	normalized := strings.Trim(strings.TrimSpace(value), "\"'")
	return normalized == "0" || strings.EqualFold(normalized, "false")
}

// parseLibvirtQEMUConfig parses the assignment syntax used by qemu.conf,
// including multi-line list values. It deliberately rejects an unterminated
// value instead of treating an incomplete security setting as safe.
func parseLibvirtQEMUConfig(data []byte) (map[string]string, error) {
	settings := make(map[string]string)
	var key string
	var value strings.Builder
	depth := 0
	quoted := false
	active := false

	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(stripLibvirtComment(rawLine))
		if line == "" {
			continue
		}
		if !active {
			left, right, ok := strings.Cut(line, "=")
			if !ok || strings.TrimSpace(left) == "" {
				return nil, fmt.Errorf("invalid qemu.conf assignment %q", line)
			}
			key = strings.TrimSpace(left)
			value.Reset()
			value.WriteString(strings.TrimSpace(right))
			active = true
		} else {
			value.WriteByte(' ')
			value.WriteString(line)
		}

		var err error
		depth, quoted, err = libvirtValueState(value.String())
		if err != nil {
			return nil, fmt.Errorf("invalid qemu.conf value for %s: %w", key, err)
		}
		if depth == 0 && !quoted {
			settings[key] = strings.TrimSpace(value.String())
			active = false
		}
	}
	if active {
		return nil, fmt.Errorf("unterminated qemu.conf value for %s", key)
	}
	return settings, nil
}

func stripLibvirtComment(line string) string {
	quoted := false
	escaped := false
	for index, char := range line {
		if escaped {
			escaped = false
			continue
		}
		if char == '\\' && quoted {
			escaped = true
			continue
		}
		if char == '"' {
			quoted = !quoted
			continue
		}
		if char == '#' && !quoted {
			return line[:index]
		}
	}
	return line
}

func libvirtValueState(value string) (depth int, quoted bool, err error) {
	escaped := false
	for _, char := range value {
		if escaped {
			escaped = false
			continue
		}
		if char == '\\' && quoted {
			escaped = true
			continue
		}
		if char == '"' {
			quoted = !quoted
			continue
		}
		if quoted {
			continue
		}
		switch char {
		case '[':
			depth++
		case ']':
			depth--
			if depth < 0 {
				return 0, false, errors.New("unexpected closing bracket")
			}
		}
	}
	if escaped {
		return depth, quoted, errors.New("trailing escape")
	}
	return depth, quoted, nil
}

func libvirtStringList(settings map[string]string, key string) ([]string, error) {
	value, ok := settings[key]
	if !ok {
		return nil, fmt.Errorf("libvirt %s must be explicitly configured in /etc/libvirt/qemu.conf", key)
	}
	var values []string
	if err := json.Unmarshal([]byte(value), &values); err != nil {
		return nil, fmt.Errorf("libvirt %s must be a string list: %w", key, err)
	}
	return values, nil
}

func containsFolded(values []string, wanted string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), wanted) {
			return true
		}
	}
	return false
}

// verifyLibvirtSecurityTranslation asks the system libvirt driver to translate
// the exact domain XML that will be defined. This checks the effective QEMU
// command construction, including the libvirt-managed seccomp policy; the
// namespace and device controls are verified from the explicit qemu.conf
// policy because libvirt does not expose them as per-domain XML elements.
func (l Libvirt) verifyLibvirtSecurityTranslation(ctx context.Context, xmlPath string) error {
	return l.verifyLibvirtSecurityTranslationArgs(ctx, []string{"domxml-to-native", "qemu-argv", xmlPath}, "domain XML")
}

func (l Libvirt) verifyLibvirtDomainSecurityTranslation(ctx context.Context, name string) error {
	if err := validateLibvirtVMName(name); err != nil {
		return err
	}
	return l.verifyLibvirtSecurityTranslationArgs(ctx, []string{"domxml-to-native", "qemu-argv", "--domain", name}, "defined domain")
}

func (l Libvirt) verifyLibvirtSecurityTranslationArgs(ctx context.Context, args []string, source string) error {
	var output bytes.Buffer
	commandArgs := append([]string{"--connect", "qemu:///system"}, args...)
	if err := command(l.Runner, ctx, "virsh", commandArgs, nil, &output, l.Err); err != nil {
		return fmt.Errorf("verify libvirt security translation: %w", err)
	}
	for _, required := range []string{
		"-sandbox on",
		"obsolete=deny",
		"elevateprivileges=deny",
		"spawn=deny",
		"resourcecontrol=deny",
	} {
		if !strings.Contains(output.String(), required) {
			return fmt.Errorf("libvirt translated QEMU command for %s is missing security option %q", source, required)
		}
	}
	return nil
}

func (l Libvirt) Start(ctx context.Context, name string) error {
	if err := validateLibvirtVMName(name); err != nil {
		return err
	}
	if err := libvirtSecurityPreflight(); err != nil {
		return err
	}
	if err := l.verifyLibvirtDomainSecurityTranslation(ctx, name); err != nil {
		return err
	}
	if err := command(l.Runner, ctx, "virsh", []string{"--connect", "qemu:///system", "start", name}, nil, l.Out, l.Err); err != nil {
		return err
	}
	readyCtx, cancel := context.WithTimeout(ctx, provisioningTimeout)
	defer cancel()
	if err := l.waitForProvisioning(readyCtx, name); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(readyCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("timed out after %s waiting for required VM provisioning: %w", provisioningTimeout, context.DeadlineExceeded)
		}
		return fmt.Errorf("required VM provisioning failed: %w", err)
	}
	return nil
}

func (l Libvirt) waitForProvisioning(ctx context.Context, name string) error {
	// The guest agent starts before cloud-init necessarily finishes. Wait for
	// that management channel first, then let cloud-init report its own failure
	// and finally require the coding-agent installer marker.
	for {
		err := l.Exec(ctx, name, []string{"/usr/bin/true"}, nil, io.Discard, io.Discard)
		if err == nil {
			break
		}
		timer := time.NewTimer(provisioningPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	if err := l.Exec(ctx, name, []string{"/usr/bin/cloud-init", "status", "--wait"}, nil, l.Out, l.Err); err != nil {
		return fmt.Errorf("cloud-init: %w", err)
	}
	if err := l.Exec(ctx, name, []string{"/usr/bin/test", "-f", provision.CodingAgentsReadyPath}, nil, io.Discard, l.Err); err != nil {
		return fmt.Errorf("coding-agent readiness marker %s is missing: %w", provision.CodingAgentsReadyPath, err)
	}
	return nil
}

func (l Libvirt) Stop(ctx context.Context, name string) error {
	if err := validateLibvirtVMName(name); err != nil {
		return err
	}
	return command(l.Runner, ctx, "virsh", []string{"--connect", "qemu:///system", "shutdown", name}, nil, l.Out, l.Err)
}

func (l Libvirt) Status(ctx context.Context, name string) (Status, error) {
	return l.status(ctx, name)
}

func (l Libvirt) status(ctx context.Context, name string) (Status, error) {
	if err := validateLibvirtVMName(name); err != nil {
		return Status{Name: name, Provider: l.Name(), Lifecycle: model.StatusUnknown}, err
	}
	var output bytes.Buffer
	if err := command(l.Runner, ctx, "virsh", []string{"--connect", "qemu:///system", "domstate", name}, nil, &output, l.Err); err != nil {
		return Status{Name: name, Provider: l.Name(), Lifecycle: model.StatusUnknown}, err
	}
	value := strings.ToLower(strings.TrimSpace(output.String()))
	lifecycle := model.StatusUnknown
	switch value {
	case "running", "idle", "paused":
		lifecycle = model.StatusRunning
	case "shut off", "shutoff", "off":
		lifecycle = model.StatusStopped
	}
	return Status{Name: name, Provider: l.Name(), Lifecycle: lifecycle, Detail: value}, nil
}

func (l Libvirt) Logs(ctx context.Context, name string, stdout, stderr io.Writer) error {
	if err := validateLibvirtVMName(name); err != nil {
		return err
	}
	return command(l.Runner, ctx, "virsh", []string{"--connect", "qemu:///system", "console", name}, nil, logging.RedactingWriter{Writer: stdout}, logging.RedactingWriter{Writer: stderr})
}

func (l Libvirt) Destroy(ctx context.Context, name string) error {
	if err := validateLibvirtVMName(name); err != nil {
		return err
	}
	if err := command(l.Runner, ctx, "virsh", []string{"--connect", "qemu:///system", "destroy", name}, nil, l.Out, l.Err); err != nil {
		// Undefine may still be safe if the domain is already stopped; the
		// caller decides whether to continue based on this explicit error.
		return err
	}
	return command(l.Runner, ctx, "virsh", []string{"--connect", "qemu:///system", "undefine", name, "--remove-all-storage"}, nil, l.Out, l.Err)
}

func (l Libvirt) Upgrade(ctx context.Context, name string, spec Spec) error {
	return l.Create(ctx, spec)
}

func (l Libvirt) Exec(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return l.execGuest(ctx, name, args, stdin, stdout, stderr)
}

// ExecAsUser keeps the Linux backend's management path consistent with Lima:
// ordinary guest commands run as the unprivileged agent account, while the
// administrative Provider.Exec path remains available for host-controlled
// operations such as package installation.
func (l Libvirt) ExecAsUser(ctx context.Context, name, user string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if user != "agent" {
		return fmt.Errorf("unsupported guest user %q", user)
	}
	if len(args) == 0 {
		return fmt.Errorf("guest command is required")
	}
	commandArgs := []string{
		"/usr/sbin/runuser", "--user", user, "--", "/usr/bin/env",
		"HOME=" + provision.AgentHome,
		"SHELL=/bin/bash",
		"PATH=" + provision.AgentManagedPath,
	}
	commandArgs = append(commandArgs, args...)
	return l.execGuest(ctx, name, commandArgs, stdin, stdout, stderr)
}

type guestAgentRequest struct {
	Execute   string `json:"execute"`
	Arguments any    `json:"arguments,omitempty"`
}

type guestAgentResponse struct {
	Return json.RawMessage  `json:"return"`
	Error  *guestAgentError `json:"error,omitempty"`
}

type guestAgentError struct {
	Class       string `json:"class"`
	Description string `json:"desc"`
}

func (e *guestAgentError) Error() string {
	if e.Description == "" {
		return "qemu guest-agent command failed: " + e.Class
	}
	return "qemu guest-agent command failed: " + e.Description
}

type guestExecArguments struct {
	Path          string   `json:"path"`
	Arguments     []string `json:"arg,omitempty"`
	InputData     string   `json:"input-data,omitempty"`
	CaptureOutput bool     `json:"capture-output"`
}

type guestExecResult struct {
	PID uint64 `json:"pid"`
}

type guestExecStatus struct {
	Exited       bool   `json:"exited"`
	ExitCode     *int   `json:"exitcode,omitempty"`
	Signal       *int   `json:"signal,omitempty"`
	OutData      string `json:"out-data,omitempty"`
	ErrData      string `json:"err-data,omitempty"`
	OutTruncated bool   `json:"out-truncated,omitempty"`
	ErrTruncated bool   `json:"err-truncated,omitempty"`
}

func (l Libvirt) execGuest(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if err := validateLibvirtVMName(name); err != nil {
		return err
	}
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		return fmt.Errorf("guest command is required")
	}
	for _, arg := range args {
		if strings.IndexByte(arg, 0) >= 0 {
			return fmt.Errorf("guest command arguments may not contain NUL bytes")
		}
	}

	input, err := readGuestInput(stdin)
	if err != nil {
		return fmt.Errorf("read guest command input: %w", err)
	}
	request := guestAgentRequest{
		Execute: "guest-exec",
		Arguments: guestExecArguments{
			Path:          args[0],
			Arguments:     append([]string(nil), args[1:]...),
			InputData:     input,
			CaptureOutput: true,
		},
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode guest command: %w", err)
	}
	result, err := l.qemuAgentCommand(ctx, name, payload)
	if err != nil {
		return err
	}
	var started guestExecResult
	if err := json.Unmarshal(result, &started); err != nil {
		return fmt.Errorf("decode guest-exec response: %w", err)
	}
	if started.PID == 0 {
		return fmt.Errorf("guest-exec returned an invalid process id")
	}

	return l.waitGuestExec(ctx, name, started.PID, stdout, stderr)
}

func readGuestInput(stdin io.Reader) (string, error) {
	if stdin == nil {
		return "", nil
	}
	data, err := io.ReadAll(stdin)
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", nil
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

func (l Libvirt) qemuAgentCommand(ctx context.Context, name string, payload []byte) (json.RawMessage, error) {
	if err := validateLibvirtVMName(name); err != nil {
		return nil, err
	}
	if !json.Valid(payload) {
		return nil, fmt.Errorf("invalid QEMU guest-agent request")
	}
	var output bytes.Buffer
	args := []string{
		"--connect", "qemu:///system", "qemu-agent-command", name,
		"--timeout", qemuGuestAgentCommandTimeout, string(payload),
	}
	if err := command(l.Runner, ctx, "virsh", args, nil, &output, l.Err); err != nil {
		return nil, fmt.Errorf("qemu-agent-command: %w", err)
	}
	var response guestAgentResponse
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		return nil, fmt.Errorf("decode qemu-agent response: %w", err)
	}
	if response.Error != nil {
		return nil, response.Error
	}
	if len(response.Return) == 0 || string(response.Return) == "null" {
		return nil, fmt.Errorf("qemu-agent response omitted return value")
	}
	return response.Return, nil
}

func (l Libvirt) waitGuestExec(ctx context.Context, name string, pid uint64, stdout, stderr io.Writer) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		payload, err := json.Marshal(guestAgentRequest{
			Execute: "guest-exec-status",
			Arguments: struct {
				PID uint64 `json:"pid"`
			}{PID: pid},
		})
		if err != nil {
			return fmt.Errorf("encode guest-exec-status request: %w", err)
		}
		result, err := l.qemuAgentCommand(ctx, name, payload)
		if err != nil {
			return err
		}
		var status guestExecStatus
		if err := json.Unmarshal(result, &status); err != nil {
			return fmt.Errorf("decode guest-exec-status response: %w", err)
		}
		if !status.Exited {
			timer := time.NewTimer(guestAgentPollInterval)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return ctx.Err()
			case <-timer.C:
			}
			continue
		}

		if err := writeGuestOutput(stdout, status.OutData); err != nil {
			return fmt.Errorf("write guest stdout: %w", err)
		}
		if err := writeGuestOutput(stderr, status.ErrData); err != nil {
			return fmt.Errorf("write guest stderr: %w", err)
		}
		if status.OutTruncated || status.ErrTruncated {
			return fmt.Errorf("guest command output was truncated")
		}
		if status.Signal != nil && *status.Signal != 0 {
			return fmt.Errorf("guest command terminated by signal %d", *status.Signal)
		}
		if status.ExitCode != nil && *status.ExitCode != 0 {
			return fmt.Errorf("guest command exited with status %d", *status.ExitCode)
		}
		return nil
	}
}

func writeGuestOutput(writer io.Writer, encoded string) error {
	if encoded == "" || writer == nil {
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("decode captured output: %w", err)
	}
	_, err = writer.Write(data)
	return err
}
