package backend

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gjpin/agent-os/internal/artifacts"
	"github.com/gjpin/agent-os/internal/execx"
	"github.com/gjpin/agent-os/internal/model"
	"github.com/gjpin/agent-os/internal/provision"
)

type backendScriptedRunner struct {
	Calls   []execx.Invocation
	Inputs  []string
	Outputs []string
	Errors  []error
}

func (r *backendScriptedRunner) Run(_ context.Context, name string, args []string, stdin io.Reader, stdout, _ io.Writer) error {
	r.Calls = append(r.Calls, execx.Invocation{Name: name, Args: append([]string(nil), args...)})
	var input bytes.Buffer
	if stdin != nil {
		_, _ = io.Copy(&input, stdin)
	}
	r.Inputs = append(r.Inputs, input.String())
	index := len(r.Calls) - 1
	if index < len(r.Outputs) && stdout != nil {
		_, _ = io.WriteString(stdout, r.Outputs[index])
	}
	if index < len(r.Errors) {
		return r.Errors[index]
	}
	return nil
}

type guestAgentTestRunner struct {
	Calls []execx.Invocation
}

type blockingRunner struct{}

func (blockingRunner) Run(ctx context.Context, _ string, _ []string, _ io.Reader, _, _ io.Writer) error {
	<-ctx.Done()
	return ctx.Err()
}

type unavailableGuestRunner struct{}

func (unavailableGuestRunner) Run(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error {
	return errors.New("guest agent is unavailable")
}

type provisioningGuestRunner struct {
	commands int
	failAt   int
}

func (r *provisioningGuestRunner) Run(_ context.Context, _ string, args []string, _ io.Reader, stdout, _ io.Writer) error {
	var request struct {
		Execute string `json:"execute"`
	}
	if err := json.Unmarshal([]byte(args[len(args)-1]), &request); err != nil {
		return err
	}
	switch request.Execute {
	case "guest-exec":
		r.commands++
		_, _ = io.WriteString(stdout, fmt.Sprintf(`{"return":{"pid":%d}}`, r.commands))
	case "guest-exec-status":
		exitCode := 0
		if r.commands == r.failAt {
			exitCode = 1
		}
		_, _ = io.WriteString(stdout, fmt.Sprintf(`{"return":{"exited":true,"exitcode":%d}}`, exitCode))
	default:
		return errors.New("unexpected request")
	}
	return nil
}

func (r *guestAgentTestRunner) Run(_ context.Context, name string, args []string, _ io.Reader, stdout, _ io.Writer) error {
	r.Calls = append(r.Calls, execx.Invocation{Name: name, Args: append([]string(nil), args...)})
	if name != "virsh" || len(args) == 0 {
		return errors.New("unexpected guest-agent invocation")
	}
	var request struct {
		Execute   string          `json:"execute"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(args[len(args)-1]), &request); err != nil {
		return err
	}
	switch request.Execute {
	case "guest-exec":
		_, _ = io.WriteString(stdout, `{"return":{"pid":42}}`)
	case "guest-exec-status":
		_, _ = io.WriteString(stdout, `{"return":{"exited":true,"exitcode":0,"out-data":"b3V0Cg==","err-data":"ZXJyCg=="}}`)
	default:
		return errors.New("unexpected guest-agent request")
	}
	return nil
}

func TestLimaUsesArgumentArrays(t *testing.T) {
	runner := &backendScriptedRunner{}
	provider := Lima{Runner: runner}
	if err := provider.Start(context.Background(), "agents"); err != nil {
		t.Fatal(err)
	}
	if len(runner.Calls) != 2 || runner.Calls[0].Name != "limactl" {
		t.Fatalf("unexpected calls: %+v", runner.Calls)
	}
	for _, call := range runner.Calls {
		for _, arg := range call.Args {
			if arg == "sh" || arg == "-c" || arg == "eval" {
				t.Fatalf("shell execution leaked into provider args: %+v", call.Args)
			}
		}
	}
	if got := runner.Calls[1].Args; strings.Join(got, " ") != "shell agents -- sudo /bin/bash -s" {
		t.Fatalf("unexpected readiness check: %+v", got)
	}
	if !strings.Contains(runner.Inputs[1], provision.ProvisioningReadyPath) || !strings.Contains(runner.Inputs[1], "agent-os-provision.service") {
		t.Fatalf("readiness check omitted the provisioning marker or service: %q", runner.Inputs[1])
	}
}

func TestLibvirtAutostartUsesPersistentDomainRegistration(t *testing.T) {
	runner := &execx.RecordingRunner{}
	provider := Libvirt{Runner: runner}
	if err := provider.EnableAutostart(context.Background(), "agents"); err != nil {
		t.Fatal(err)
	}
	if err := provider.DisableAutostart(context.Background(), "agents"); err != nil {
		t.Fatal(err)
	}
	if len(runner.Calls) != 2 {
		t.Fatalf("unexpected calls: %+v", runner.Calls)
	}
	if got := strings.Join(runner.Calls[0].Args, " "); got != "--connect qemu:///system autostart agents" {
		t.Fatalf("unexpected enable command: %s", got)
	}
	if got := strings.Join(runner.Calls[1].Args, " "); got != "--connect qemu:///system autostart --disable agents" {
		t.Fatalf("unexpected disable command: %s", got)
	}
}

func TestLimaAutostartRequiresSupportedVersionAndUsesBootCondition(t *testing.T) {
	runner := &backendScriptedRunner{Outputs: []string{"limactl version 2.2.0\n"}}
	provider := Lima{Runner: runner}
	if err := provider.EnableAutostart(context.Background(), "agents"); err != nil {
		t.Fatal(err)
	}
	if len(runner.Calls) != 2 || strings.Join(runner.Calls[1].Args, " ") != "autostart enable --condition=boot agents" {
		t.Fatalf("unexpected Lima autostart calls: %+v", runner.Calls)
	}
	if err := provider.DisableAutostart(context.Background(), "agents"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(runner.Calls[2].Args, " "); got != "autostart disable agents" {
		t.Fatalf("unexpected Lima disable command: %s", got)
	}

	runner = &backendScriptedRunner{Outputs: []string{"limactl version 2.1.0\n"}}
	err := (Lima{Runner: runner}).EnableAutostart(context.Background(), "agents")
	if err == nil || !strings.Contains(err.Error(), "upgrade to Lima 2.2 or newer") {
		t.Fatalf("expected actionable old-Lima error, got %v", err)
	}
	if len(runner.Calls) != 1 {
		t.Fatalf("old Lima was given an autostart command: %+v", runner.Calls)
	}
}

func TestLibvirtAutostartArtifactsAreValidatedAndShellFree(t *testing.T) {
	c := model.DefaultConfig("/state")
	c.VMName = "host-boot-vm"
	runner := &backendScriptedRunner{}
	provider := Libvirt{Runner: runner}
	if err := provider.ConfigureAutostart(context.Background(), Spec{Config: c}); err != nil {
		t.Fatal(err)
	}
	if len(runner.Calls) != 5 {
		t.Fatalf("unexpected host artifact calls: %+v", runner.Calls)
	}
	rules := runner.Inputs[1]
	unit := runner.Inputs[2]
	if !strings.Contains(rules, "table ip "+artifacts.ForwardingTableName(c.VMName)) || !strings.Contains(rules, "dnat to "+artifacts.LibvirtGuestAddress(c.VMName)+":6768") {
		t.Fatalf("host rules are not for the stable VM target: %s", rules)
	}
	for _, value := range []string{unit, rules} {
		if strings.Contains(value, "sh -c") || strings.Contains(value, "eval ") || strings.Contains(value, c.VMName) {
			t.Fatalf("host autostart artifact contains unsafe/user-controlled text: %s", value)
		}
	}
	for _, call := range runner.Calls {
		for _, arg := range call.Args {
			if arg == "sh" || arg == "-c" || arg == "eval" {
				t.Fatalf("shell execution leaked into autostart command: %+v", call)
			}
		}
	}

	c.VMName = "bad vm"
	runner = &backendScriptedRunner{}
	if err := (Libvirt{Runner: runner}).ConfigureAutostart(context.Background(), Spec{Config: c}); err == nil {
		t.Fatal("invalid VM name was accepted for host autostart")
	}
	if len(runner.Calls) != 0 {
		t.Fatalf("invalid host autostart configuration ran commands: %+v", runner.Calls)
	}
}

func TestLimaExecAsRootUsesProviderSudoWrapper(t *testing.T) {
	runner := &execx.RecordingRunner{}
	if err := (Lima{Runner: runner}).ExecAsRoot(context.Background(), "agents", []string{"/bin/bash", "-s"}, strings.NewReader("true\n"), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(runner.Calls) != 1 || strings.Join(runner.Calls[0].Args, " ") != "shell agents -- sudo /bin/bash -s" {
		t.Fatalf("unexpected Lima root execution: %+v", runner.Calls)
	}
}

func TestLimaStartSurfacesProvisioningReadinessFailures(t *testing.T) {
	runner := &backendScriptedRunner{Errors: []error{nil, errors.New("marker absent")}}
	err := (Lima{Runner: runner}).Start(context.Background(), "agents")
	if err == nil || !strings.Contains(err.Error(), "readiness marker") {
		t.Fatalf("expected missing-marker failure, got %v", err)
	}

	runner = &backendScriptedRunner{Errors: []error{errors.New("provision script failed")}}
	err = (Lima{Runner: runner}).Start(context.Background(), "agents")
	if err == nil || !strings.Contains(err.Error(), "required Lima provisioning failed") {
		t.Fatalf("expected Lima provisioning failure, got %v", err)
	}
}

func TestLimaStartHonorsTimeoutAndCancellation(t *testing.T) {
	originalTimeout := provisioningTimeout
	provisioningTimeout = 5 * time.Millisecond
	t.Cleanup(func() { provisioningTimeout = originalTimeout })

	err := (Lima{Runner: blockingRunner{}}).Start(context.Background(), "agents")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected provisioning timeout, got %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = (Lima{Runner: blockingRunner{}}).Start(ctx, "agents")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func TestLibvirtProvisioningRequiresCloudInitAndMarker(t *testing.T) {
	runner := &provisioningGuestRunner{}
	if err := (Libvirt{Runner: runner}).waitForProvisioning(context.Background(), "agents"); err != nil {
		t.Fatal(err)
	}
	if runner.commands != 3 {
		t.Fatalf("expected guest-agent, cloud-init, and marker commands, got %d", runner.commands)
	}

	runner = &provisioningGuestRunner{failAt: 2}
	err := (Libvirt{Runner: runner}).waitForProvisioning(context.Background(), "agents")
	if err == nil || !strings.Contains(err.Error(), "cloud-init") {
		t.Fatalf("expected cloud-init failure, got %v", err)
	}

	runner = &provisioningGuestRunner{failAt: 3}
	err = (Libvirt{Runner: runner}).waitForProvisioning(context.Background(), "agents")
	if err == nil || !strings.Contains(err.Error(), "readiness marker") {
		t.Fatalf("expected marker failure, got %v", err)
	}
}

func TestLibvirtProvisioningWaitHonorsTimeoutAndCancellation(t *testing.T) {
	originalInterval := provisioningPollInterval
	provisioningPollInterval = time.Millisecond
	t.Cleanup(func() { provisioningPollInterval = originalInterval })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	err := (Libvirt{Runner: unavailableGuestRunner{}}).waitForProvisioning(ctx, "agents")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected timeout, got %v", err)
	}

	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	err = (Libvirt{Runner: unavailableGuestRunner{}}).waitForProvisioning(ctx, "agents")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func TestAgentUserExecutionSetsManagedEnvironment(t *testing.T) {
	limaRunner := &execx.RecordingRunner{}
	if err := (Lima{Runner: limaRunner}).ExecAsUser(context.Background(), "agents", "agent", []string{"codex", "login"}, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	limaArgs := strings.Join(limaRunner.Calls[0].Args, " ")
	for _, expected := range []string{"--workdir", provision.AgentHome, "-H", "HOME=" + provision.AgentHome, "SHELL=/bin/bash", "PATH=" + provision.AgentManagedPath, "codex login"} {
		if !strings.Contains(limaArgs, expected) {
			t.Errorf("Lima agent execution omits %q: %s", expected, limaArgs)
		}
	}

	libvirtRunner := &guestAgentTestRunner{}
	if err := (Libvirt{Runner: libvirtRunner}).ExecAsUser(context.Background(), "agents", "agent", []string{"codex", "login"}, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	var request struct {
		Arguments struct {
			Path string   `json:"path"`
			Args []string `json:"arg"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(libvirtRunner.Calls[0].Args[len(libvirtRunner.Calls[0].Args)-1]), &request); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(request.Arguments.Args, " ")
	if request.Arguments.Path != "/usr/sbin/runuser" || !strings.Contains(joined, "HOME="+provision.AgentHome) || !strings.Contains(joined, "PATH="+provision.AgentManagedPath) || !strings.HasSuffix(joined, "codex login") {
		t.Fatalf("unexpected libvirt agent execution: path=%q args=%q", request.Arguments.Path, joined)
	}
}

func TestLibvirtForwardingUsesWireGuardInterface(t *testing.T) {
	runner := &execx.RecordingRunner{}
	c := model.DefaultConfig("/state")
	c.AccessMode = model.AccessWireGuard
	c.WireGuardInterface = "wg0"
	c.WireGuardAddress = "10.64.0.2/32"
	provider := Libvirt{Runner: runner}
	if err := provider.ConfigureForwarding(context.Background(), Spec{Config: c}); err != nil {
		t.Fatal(err)
	}
	if len(runner.Calls) < 5 {
		t.Fatalf("expected interface check, sysctl, cleanup, and nft install: %+v", runner.Calls)
	}
	if runner.Calls[0].Name != "sudo" || len(runner.Calls[0].Args) < 5 || runner.Calls[0].Args[0] != "ip" || runner.Calls[0].Args[4] != "wg0" {
		t.Fatalf("WireGuard interface was not checked: %+v", runner.Calls[0])
	}
	last := runner.Calls[len(runner.Calls)-1]
	if last.Name != "sudo" || len(last.Args) != 3 || last.Args[0] != "nft" || last.Args[1] != "-f" || last.Args[2] != "-" {
		t.Fatalf("nft rules were not installed: %+v", last)
	}
}

func TestLibvirtWireGuardForwardingUsesGuestTarget(t *testing.T) {
	runner := &backendScriptedRunner{}
	c := model.DefaultConfig("/state")
	c.VMName = "wireguard-vm"
	c.AccessMode = model.AccessWireGuard
	c.WireGuardInterface = "wg0"
	c.WireGuardAddress = "10.64.0.2/32"
	provider := Libvirt{Runner: runner}

	if err := provider.ConfigureForwarding(context.Background(), Spec{Config: c}); err != nil {
		t.Fatal(err)
	}
	if len(runner.Calls) != 5 {
		t.Fatalf("unexpected forwarding calls: %+v", runner.Calls)
	}
	if runner.Calls[0].Name != "sudo" || strings.Join(runner.Calls[0].Args, " ") != "ip link show dev wg0" {
		t.Fatalf("unexpected WireGuard check: %+v", runner.Calls[0])
	}
	if got := runner.Calls[3].Args; len(got) != 3 || got[0] != "sysctl" || got[1] != "-w" || got[2] != "net.ipv4.ip_forward=1" {
		t.Fatalf("unexpected forwarding sysctl: %+v", runner.Calls[3])
	}
	rules := runner.Inputs[4]
	guest := artifacts.LibvirtGuestAddress(c.VMName)
	if !strings.Contains(rules, "ip daddr 10.64.0.2") {
		t.Fatalf("rules omitted the host WireGuard endpoint: %s", rules)
	}
	if !strings.Contains(rules, "dnat to "+guest+":"+"6768") {
		t.Fatalf("rules did not target the guest address %s: %s", guest, rules)
	}
	if strings.Contains(rules, "dnat to 10.64.0.2:6768") {
		t.Fatalf("rules attempted to DNAT to the host WireGuard address: %s", rules)
	}
}

func TestLibvirtLocalForwardingDoesNotCheckWireGuard(t *testing.T) {
	runner := &backendScriptedRunner{}
	c := model.DefaultConfig("/state")
	c.VMName = "local-vm"
	c.AccessMode = model.AccessLocal
	provider := Libvirt{Runner: runner}

	if err := provider.ConfigureForwarding(context.Background(), Spec{Config: c}); err != nil {
		t.Fatal(err)
	}
	if len(runner.Calls) != 4 {
		t.Fatalf("unexpected local forwarding calls: %+v", runner.Calls)
	}
	for _, call := range runner.Calls {
		if strings.Contains(strings.Join(call.Args, " "), "ip link") {
			t.Fatalf("local forwarding checked a WireGuard interface: %+v", runner.Calls)
		}
	}
	rules := runner.Inputs[len(runner.Inputs)-1]
	guest := artifacts.LibvirtGuestAddress(c.VMName)
	if !strings.Contains(rules, "ip daddr 127.0.0.1 tcp dport 6768 dnat to "+guest+":6768") {
		t.Fatalf("local forwarding omitted the loopback DNAT: %s", rules)
	}
	if strings.Contains(rules, "iifname") || strings.Contains(rules, "10.64.") {
		t.Fatalf("local forwarding unexpectedly contains WireGuard rules: %s", rules)
	}
}

func TestLibvirtForwardingValidatesBeforeHostMutation(t *testing.T) {
	runner := &backendScriptedRunner{}
	c := model.DefaultConfig("/state")
	c.VMName = "bad vm"
	if err := (Libvirt{Runner: runner}).ConfigureForwarding(context.Background(), Spec{Config: c}); err == nil {
		t.Fatal("expected invalid VM name to be rejected")
	}
	if len(runner.Calls) != 0 {
		t.Fatalf("invalid forwarding config ran host commands: %+v", runner.Calls)
	}
}

func TestLibvirtForwardingCleanupErrorsAreReturned(t *testing.T) {
	c := model.DefaultConfig("/state")
	c.VMName = "cleanup-vm"
	runner := &backendScriptedRunner{Errors: []error{nil, errors.New("delete failed")}}
	provider := Libvirt{Runner: runner}
	if err := provider.ConfigureForwarding(context.Background(), Spec{Config: c}); err == nil || !strings.Contains(err.Error(), "remove Orca forwarding rules") {
		t.Fatalf("expected stale forwarding cleanup error, got %v", err)
	}
	if len(runner.Calls) != 2 {
		t.Fatalf("forwarding continued after cleanup failure: %+v", runner.Calls)
	}

	runner = &backendScriptedRunner{Errors: []error{nil, errors.New("delete failed")}}
	provider = Libvirt{Runner: runner}
	if err := provider.RemoveForwarding(context.Background(), Spec{Config: c}); err == nil || !strings.Contains(err.Error(), "remove Orca forwarding rules") {
		t.Fatalf("expected removal error, got %v", err)
	}
}

func TestLibvirtForwardingCleanupToleratesMissingTable(t *testing.T) {
	c := model.DefaultConfig("/state")
	c.VMName = "missing-table-vm"
	runner := &backendScriptedRunner{Errors: []error{errors.New("table does not exist")}}
	if err := (Libvirt{Runner: runner}).RemoveForwarding(context.Background(), Spec{Config: c}); err != nil {
		t.Fatalf("missing forwarding table should be safe to remove: %v", err)
	}
	if len(runner.Calls) != 1 {
		t.Fatalf("cleanup tried to delete a table that was not present: %+v", runner.Calls)
	}
}

func TestLibvirtForwardingCleanupHonorsCancellation(t *testing.T) {
	c := model.DefaultConfig("/state")
	c.VMName = "cancelled-vm"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := &backendScriptedRunner{}
	if err := (Libvirt{Runner: runner}).RemoveForwarding(ctx, Spec{Config: c}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation from forwarding cleanup, got %v", err)
	}
	if len(runner.Calls) != 0 {
		t.Fatalf("cancelled cleanup ran host commands: %+v", runner.Calls)
	}
}

func TestLibvirtCreateBindsOrcaToGuestAndAddsGuestAgent(t *testing.T) {
	stateDir := t.TempDir()
	c := model.DefaultConfig(stateDir)
	c.VMName = "wireguard-create"
	c.AccessMode = model.AccessWireGuard
	c.WireGuardInterface = "wg0"
	c.WireGuardAddress = "10.64.0.2/32"
	if err := (Libvirt{}).Create(context.Background(), Spec{Config: c, Architecture: "x86_64", DryRun: true}); err != nil {
		t.Fatal(err)
	}
	artifactDir := filepath.Join(stateDir, "v1", "vms", c.VMName, "artifacts")
	xmlBytes, err := os.ReadFile(filepath.Join(artifactDir, "domain.xml"))
	if err != nil {
		t.Fatal(err)
	}
	xml := string(xmlBytes)
	if !strings.Contains(xml, `<channel type="unix">`) || strings.Count(xml, `name="org.qemu.guest_agent.0"`) != 1 {
		t.Fatalf("libvirt XML omitted the guest-agent channel: %s", xml)
	}
	serviceBytes, err := os.ReadFile(filepath.Join(artifactDir, "orca.service"))
	if err != nil {
		t.Fatal(err)
	}
	guest := artifacts.LibvirtGuestAddress(c.VMName)
	service := string(serviceBytes)
	if !strings.Contains(service, "ORCA_BIND_ADDRESS="+guest) || strings.Contains(service, "ORCA_BIND_ADDRESS=10.64.0.2") {
		t.Fatalf("Orca was not bound to the guest address: %s", service)
	}
	if !strings.Contains(service, "--pairing-address 10.64.0.2") {
		t.Fatalf("WireGuard forwarding endpoint was not retained as the pairing endpoint: %s", service)
	}
	userDataBytes, err := os.ReadFile(filepath.Join(artifactDir, "user-data"))
	if err != nil {
		t.Fatal(err)
	}
	userData := string(userDataBytes)
	if !strings.Contains(userData, "- qemu-guest-agent") || !strings.Contains(userData, "qemu-guest-agent.service") {
		t.Fatalf("user-data did not provision and enable qemu-guest-agent: %s", userData)
	}
}

func TestLibvirtExecUsesGuestAgentJSONAndStreamsOutput(t *testing.T) {
	runner := &guestAgentTestRunner{}
	var stdout, stderr bytes.Buffer
	input := bytes.NewBufferString("input\n")
	if err := (Libvirt{Runner: runner}).Exec(context.Background(), "agents", []string{"printf", "hello world"}, input, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if len(runner.Calls) != 2 {
		t.Fatalf("expected guest-exec and guest-exec-status, got %+v", runner.Calls)
	}
	first := runner.Calls[0]
	if first.Name != "virsh" || len(first.Args) != 7 || first.Args[0] != "--connect" || first.Args[1] != "qemu:///system" || first.Args[2] != "qemu-agent-command" || first.Args[3] != "agents" || first.Args[4] != "--timeout" || first.Args[5] != qemuGuestAgentCommandTimeout {
		t.Fatalf("unexpected qemu-agent-command arguments: %+v", first)
	}
	var request struct {
		Execute   string `json:"execute"`
		Arguments struct {
			Path          string   `json:"path"`
			Args          []string `json:"arg"`
			InputData     string   `json:"input-data"`
			CaptureOutput bool     `json:"capture-output"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(first.Args[len(first.Args)-1]), &request); err != nil {
		t.Fatal(err)
	}
	if request.Execute != "guest-exec" || request.Arguments.Path != "printf" || len(request.Arguments.Args) != 1 || request.Arguments.Args[0] != "hello world" || request.Arguments.InputData != base64.StdEncoding.EncodeToString([]byte("input\n")) || !request.Arguments.CaptureOutput {
		t.Fatalf("unexpected guest-exec request: %+v", request)
	}
	if stdout.String() != "out\n" || stderr.String() != "err\n" {
		t.Fatalf("unexpected guest output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	var status struct {
		Execute   string `json:"execute"`
		Arguments struct {
			PID uint64 `json:"pid"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(runner.Calls[1].Args[len(runner.Calls[1].Args)-1]), &status); err != nil {
		t.Fatal(err)
	}
	if status.Execute != "guest-exec-status" || status.Arguments.PID != 42 {
		t.Fatalf("unexpected guest-exec-status request: %+v", status)
	}
}

func TestLibvirtExecValidatesArguments(t *testing.T) {
	runner := &guestAgentTestRunner{}
	provider := Libvirt{Runner: runner}
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "", args: []string{"true"}},
		{name: "agents", args: nil},
		{name: "agents", args: []string{"true\x00bad"}},
	} {
		if err := provider.Exec(context.Background(), test.name, test.args, nil, nil, nil); err == nil {
			t.Errorf("Exec(%q, %q) accepted invalid arguments", test.name, test.args)
		}
	}
	if len(runner.Calls) != 0 {
		t.Fatalf("invalid guest commands ran virsh: %+v", runner.Calls)
	}
}

func TestLibvirtSecurityPreflightRejectsExplicitlyDisabledControls(t *testing.T) {
	if err := validateLibvirtSecurityConfig([]byte(`
seccomp_sandbox = 1
namespaces = [ "mount", "ipc", "uts", "net", "pid" ]
cgroup_controllers = [ "cpu", "memory", "devices" ]
`)); err != nil {
		t.Fatalf("accepted valid libvirt security settings: %v", err)
	}

	for _, test := range []struct {
		name  string
		value string
	}{
		{name: "seccomp", value: "seccomp_sandbox = 0"},
		{name: "mount namespace", value: `namespaces = [ "ipc", "uts", "net", "pid" ]`},
		{name: "device cgroup", value: `cgroup_controllers = [ "cpu", "memory" ]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateLibvirtSecurityConfig([]byte(test.value)); err == nil {
				t.Fatalf("accepted disabled %s control", test.name)
			}
		})
	}
}

func TestLibvirtSecurityConfigRequiresExplicitControls(t *testing.T) {
	base := `
seccomp_sandbox = 1
namespaces = [ "mount" ]
cgroup_controllers = [ "cpu", "devices" ]
`
	for _, test := range []struct {
		name string
		data string
		want string
	}{
		{name: "missing seccomp", data: "namespaces = [ \"mount\" ]\ncgroup_controllers = [ \"devices\" ]", want: "seccomp"},
		{name: "missing mount namespace", data: "seccomp_sandbox = 1\nnamespaces = []\ncgroup_controllers = [ \"devices\" ]", want: "mount namespace"},
		{name: "missing device controller", data: "seccomp_sandbox = 1\nnamespaces = [ \"mount\" ]\ncgroup_controllers = [ \"cpu\" ]", want: "device cgroup"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateLibvirtSecurityConfig([]byte(test.data)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q error, got %v", test.want, err)
			}
		})
	}

	if err := validateLibvirtSecurityConfig([]byte(base)); err != nil {
		t.Fatalf("rejected explicit security policy: %v", err)
	}

	for _, data := range []string{
		base + `security_default_confined = 0
`,
		base + `security_default_confined = false
`,
		base + `security_driver = []
`,
		base + `security_driver = [ "none" ]
`,
	} {
		if err := validateLibvirtSecurityConfig([]byte(data)); err == nil {
			t.Fatalf("accepted a disabled libvirt security setting: %q", data)
		}
	}
}

func TestLibvirtSecurityPreflightFailsClosedWhenConfigIsMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "qemu.conf")
	if err := libvirtSecurityPreflightAt(path); err == nil || !strings.Contains(err.Error(), "not explicit") {
		t.Fatalf("missing libvirt security configuration was accepted: %v", err)
	}
}

func TestLibvirtSecurityConfigParsesMultilineLists(t *testing.T) {
	data := []byte(`
seccomp_sandbox = 1
namespaces = [
  "ipc",
  "mount"
]
cgroup_controllers = [
  "cpu",
  "devices"
]
`)
	if err := validateLibvirtSecurityConfig(data); err != nil {
		t.Fatalf("rejected multiline security policy: %v", err)
	}
}

func TestLibvirtSecurityTranslationRequiresSandboxPolicy(t *testing.T) {
	runner := &backendScriptedRunner{Outputs: []string{`/usr/bin/qemu-system-x86_64 -sandbox on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny`}}
	provider := Libvirt{Runner: runner}
	if err := provider.verifyLibvirtSecurityTranslation(context.Background(), "/state/domain.xml"); err != nil {
		t.Fatal(err)
	}
	if len(runner.Calls) != 1 || runner.Calls[0].Name != "virsh" || !strings.Contains(strings.Join(runner.Calls[0].Args, " "), "domxml-to-native qemu-argv /state/domain.xml") {
		t.Fatalf("unexpected libvirt security verification call: %+v", runner.Calls)
	}

	runner = &backendScriptedRunner{Outputs: []string{"/usr/bin/qemu-system-x86_64"}}
	if err := (Libvirt{Runner: runner}).verifyLibvirtSecurityTranslation(context.Background(), "/state/domain.xml"); err == nil {
		t.Fatal("accepted a QEMU translation without the sandbox policy")
	}
}

func TestLibvirtStartVerifiesDefinedDomainSecurity(t *testing.T) {
	// Start performs the host qemu.conf preflight before reaching virsh, so the
	// domain-translation helper is tested directly with the same runner shape.
	runner := &backendScriptedRunner{Outputs: []string{`/usr/bin/qemu-system-x86_64 -sandbox on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny`}}
	provider := Libvirt{Runner: runner}
	if err := provider.verifyLibvirtDomainSecurityTranslation(context.Background(), "agents"); err != nil {
		t.Fatal(err)
	}
	if len(runner.Calls) != 1 || !strings.Contains(strings.Join(runner.Calls[0].Args, " "), "domxml-to-native qemu-argv --domain agents") {
		t.Fatalf("unexpected defined-domain security verification call: %+v", runner.Calls)
	}
}

func TestDryRunProvidersIncludeAgentInstructions(t *testing.T) {
	content := "# embedded from the release\n\nDo not depend on the caller's cwd.\n"
	providers := []struct {
		name     string
		provider Provider
		artifact string
	}{
		{name: "libvirt", provider: Libvirt{}, artifact: "user-data"},
		{name: "lima", provider: Lima{}, artifact: "lima.yaml"},
	}

	for _, test := range providers {
		t.Run(test.name, func(t *testing.T) {
			stateDir := t.TempDir()
			config := model.DefaultConfig(stateDir)
			config.VMName = "dry-run-" + test.name
			spec := Spec{
				Config:            config,
				Architecture:      "x86_64",
				AgentInstructions: content,
				DryRun:            true,
			}
			if err := test.provider.Create(context.Background(), spec); err != nil {
				t.Fatal(err)
			}
			artifactPath := filepath.Join(stateDir, "v1", "vms", config.VMName, "artifacts", test.artifact)
			data, err := os.ReadFile(artifactPath)
			if err != nil {
				t.Fatal(err)
			}
			artifact := string(data)
			for _, expected := range []string{
				"# embedded from the release",
				"Do not depend on the caller",
				provision.AgentInstructionsCanonicalPath,
				provision.AgentInstructionsOpencodePath,
				provision.AgentInstructionsCodexPath,
				provision.AgentInstructionsClaudePath,
				provision.AgentInstructionsPiPath,
				provision.AgentInstructionsCopilotPath,
				"rm -rf --",
				"ln -s --",
				"dnf install -y dnf-plugins-core",
				"dnf config-manager addrepo --from-repofile=https://rpm.releases.hashicorp.com/fedora/hashicorp.repo",
				"dnf install -y terraform",
				"dnf install -y adoptium-temurin-java-repository",
				"dnf config-manager setopt adoptium-temurin-java-repository.enabled=1",
				"dnf install -y temurin-25-jdk",
				"--prefix \"$agent_home/.local\" chrome-devtools-mcp@latest skills@latest",
				"chrome-devtools chrome-devtools-mcp",
				provision.DefaultChromeDevToolsSkillURL,
			} {
				if !strings.Contains(artifact, expected) {
					t.Errorf("artifact omits %q", expected)
				}
			}
		})
	}
}
