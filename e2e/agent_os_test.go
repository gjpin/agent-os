//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gjpin/agent-os/internal/backend"
	"github.com/gjpin/agent-os/internal/credentials"
	"github.com/gjpin/agent-os/internal/execx"
	"github.com/gjpin/agent-os/internal/host"
	"github.com/gjpin/agent-os/internal/model"
	"github.com/gjpin/agent-os/internal/profile"
	"github.com/gjpin/agent-os/internal/provision"
	"golang.org/x/crypto/ssh"
)

const (
	e2eCommandTimeout  = 60 * time.Minute
	stopTimeout        = 2 * time.Minute
	statusPollInterval = 2 * time.Second
)

var documentedAgentOSEnv = []string{
	"AGENT_OS_VM_NAME",
	"AGENT_OS_VM_CPUS",
	"AGENT_OS_VM_MEMORY_MIB",
	"AGENT_OS_VM_DISK_GIB",
	"AGENT_OS_PROFILE_DISK_GIB",
	"AGENT_OS_ACCESS_MODE",
	"AGENT_OS_ORCA_PORT",
	"AGENT_OS_WIREGUARD_INTERFACE",
	"AGENT_OS_WIREGUARD_ADDRESS",
	"AGENT_OS_REPOSITORY_KEY_PATH",
	"AGENT_OS_ALLOWED_CIDRS",
	"AGENT_OS_STATE_DIR",
	"AGENT_OS_LOG_FORMAT",
	"AGENT_OS_PACKAGES",
	"AGENT_OS_SKILLS",
}

// TestAgentOSE2E is intentionally guarded by an environment variable in
// addition to the build tag. A developer who explicitly compiles the package
// should still have to opt in before a real VM is created.
func TestAgentOSE2E(t *testing.T) {
	if os.Getenv("AGENT_OS_E2E") != "1" {
		t.Skip("real VM E2E tests require AGENT_OS_E2E=1")
	}

	info := host.Detect()
	if !supportedE2EHost(info) {
		t.Skipf("real VM E2E tests are unsupported on %s/%s", info.OS, info.Architecture)
	}

	root := moduleRoot(t)
	instructionDistro := os.Getenv("AGENT_OS_E2E_DISTRO")
	if instructionDistro == "" {
		instructionDistro = string(model.DistributionFedora)
	}
	expectedInstructions, err := os.ReadFile(filepath.Join(root, "internal", "instructions", instructionDistro, "AGENTS.md"))
	if err != nil {
		t.Fatalf("read embedded instruction source: %v", err)
	}

	h := newHarness(t, root, info, string(expectedInstructions))
	checkHostTools(t, info)
	h.preflightProvider(t)
	h.validateAndInspectConfig(t)

	h.created = true
	h.mustCLI(t, e2eCommandTimeout, "create", "--distro", string(h.distribution), h.vmName)
	h.assertStatus(t, "stopped")

	h.vmRunning = true
	h.mustCLI(t, e2eCommandTimeout, "start", h.vmName)
	h.assertStatus(t, "running")
	h.mustCLI(t, e2eCommandTimeout, "verify", h.vmName)
	h.waitForHostPort(t)
	h.assertGuestHealth(t)
	h.mustCLI(t, e2eCommandTimeout, "upgrade", "--yes", h.vmName)
	h.waitForHostPort(t)
	h.assertGuestHealth(t)

	h.mustWriteSentinel(t)
	h.mustCLI(t, stopTimeout, "stop", h.vmName)
	h.waitForLifecycle(t, "stopped")
	h.vmRunning = false
	h.assertStatus(t, "stopped")

	h.vmRunning = true
	h.mustCLI(t, e2eCommandTimeout, "start", h.vmName)
	h.assertStatus(t, "running")
	h.waitForHostPort(t)
	h.assertGuestHealth(t)
	h.assertSentinel(t)

	h.mustCLI(t, stopTimeout, "stop", h.vmName)
	h.waitForLifecycle(t, "stopped")
	h.vmRunning = false
	h.assertStatus(t, "stopped")

	if h.keepVM {
		h.printRetentionNotice(t)
		return
	}

	h.mustCLI(t, e2eCommandTimeout, "destroy", "--yes", "--purge-profiles", h.vmName)
	h.assertDestroyed(t)
	h.destroyed = true
}

func supportedE2EHost(info host.Info) bool {
	return (info.OS == "linux" && info.Architecture == "amd64") ||
		(info.OS == "darwin" && info.Architecture == "arm64")
}

func checkHostTools(t *testing.T, info host.Info) {
	t.Helper()
	required := []string{"limactl"}
	if info.OS == "linux" {
		required = append(required, "qemu-system-x86_64")
	}
	missing := make([]string, 0)
	for _, name := range required {
		if _, err := exec.LookPath(name); err != nil {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("supported Lima host is missing required provider tooling: %s", strings.Join(missing, ", "))
	}
}

type harness struct {
	t                    *testing.T
	root                 string
	limaHome             string
	home                 string
	configPath           string
	stateDir             string
	binaryPath           string
	keyPath              string
	vmName               string
	port                 int
	providerName         string
	distribution         model.Distribution
	expectedInstructions string
	env                  []string
	runner               *isolatedRunner
	provider             backend.Provider
	keepVM               bool
	created              bool
	vmRunning            bool
	destroyed            bool
	noticePrinted        bool
}

func newHarness(t *testing.T, root string, info host.Info, instructions string) *harness {
	t.Helper()
	tmpRoot, err := os.MkdirTemp("", "agent-os-e2e-")
	if err != nil {
		t.Fatalf("create isolated E2E directory: %v", err)
	}
	limaHome, err := os.MkdirTemp("/tmp", "aoe-")
	if err != nil {
		_ = os.RemoveAll(tmpRoot)
		t.Fatalf("create short isolated Lima directory: %v", err)
	}
	removeHarnessPaths := func() {
		_ = os.RemoveAll(tmpRoot)
		_ = os.RemoveAll(limaHome)
	}
	ready := false
	defer func() {
		if !ready {
			removeHarnessPaths()
		}
	}()

	home := filepath.Join(tmpRoot, "home")
	configHome := filepath.Join(tmpRoot, "config-home")
	stateHome := filepath.Join(tmpRoot, "state-home")
	for _, dir := range []string{home, configHome, stateHome} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			removeHarnessPaths()
			t.Fatalf("create isolated E2E directory %s: %v", dir, err)
		}
	}
	stateDir := filepath.Join(stateHome, "agent-os")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		removeHarnessPaths()
		t.Fatalf("create isolated state directory: %v", err)
	}

	vmName := uniqueVMName(t)
	distributionValue := os.Getenv("AGENT_OS_E2E_DISTRO")
	if distributionValue == "" {
		distributionValue = string(model.DistributionFedora)
	}
	distribution, err := model.ParseDistribution(distributionValue)
	if err != nil {
		removeHarnessPaths()
		t.Fatal(err)
	}
	port := freeLocalPort(t)
	keyPath := filepath.Join(tmpRoot, "repository-key")
	writeRepositoryKey(t, keyPath)
	configPath := filepath.Join(tmpRoot, "config.yaml")
	writeE2EConfig(t, configPath, vmName, port, stateDir, keyPath)
	binaryPath := filepath.Join(tmpRoot, "agent-os")
	buildBinary(t, root, binaryPath)

	env := isolatedEnvironment(home, configHome, stateHome, limaHome)
	runner := &isolatedRunner{env: env}
	provider, err := host.Provider(info, runner, io.Discard, io.Discard)
	if err != nil {
		removeHarnessPaths()
		t.Fatalf("select Lima provider: %v", err)
	}

	h := &harness{
		t:                    t,
		root:                 tmpRoot,
		limaHome:             limaHome,
		home:                 home,
		configPath:           configPath,
		stateDir:             stateDir,
		binaryPath:           binaryPath,
		keyPath:              keyPath,
		vmName:               vmName,
		port:                 port,
		providerName:         provider.Name(),
		distribution:         distribution,
		expectedInstructions: instructions,
		env:                  env,
		runner:               runner,
		provider:             provider,
		keepVM:               os.Getenv("AGENT_OS_E2E_KEEP_VM") == "1",
	}
	t.Cleanup(h.cleanup)
	ready = true
	return h
}

func (h *harness) preflightProvider(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.provider.Available(ctx); err != nil {
		t.Fatalf("%s provider tooling is unavailable: %v", h.providerName, err)
	}
}

func (h *harness) validateAndInspectConfig(t *testing.T) {
	t.Helper()
	h.mustCLI(t, time.Minute, "config", "validate")
	output := h.mustCLI(t, time.Minute, "--log-format", "json", "config", "show", "--effective")
	var result struct {
		Values  map[string]any    `json:"values"`
		Sources map[string]string `json:"sources"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("decode effective configuration: %v\n%s", err, output)
	}
	if got := result.Values["vm.name"]; got != h.vmName {
		t.Fatalf("effective VM name is %v, want %q", got, h.vmName)
	}
	if got := result.Values["state.dir"]; got != h.stateDir {
		t.Fatalf("effective state directory is %v, want %q", got, h.stateDir)
	}
	if got, ok := result.Values["orca.port"].(float64); !ok || int(got) != h.port {
		t.Fatalf("effective Orca port is %v, want %d", result.Values["orca.port"], h.port)
	}
	if got := result.Values["repository.key_path"]; got != "<redacted>" {
		t.Fatalf("effective repository key was not redacted: %v", got)
	}
	for _, key := range []string{"vm.name", "orca.port", "repository.key_path", "state.dir"} {
		if got := result.Sources[key]; got != "config-file" {
			t.Fatalf("effective configuration source for %s is %q, want config-file", key, got)
		}
	}
}

func (h *harness) mustCLI(t *testing.T, timeout time.Duration, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	fullArgs := make([]string, 0, len(args)+2)
	fullArgs = append(fullArgs, "--config", h.configPath)
	fullArgs = append(fullArgs, args...)
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, h.binaryPath, fullArgs...)
	cmd.Dir = filepath.Dir(h.binaryPath)
	cmd.Env = h.env
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		t.Fatalf("agent-os %s failed: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

func (h *harness) assertStatus(t *testing.T, wanted string) {
	t.Helper()
	output := h.mustCLI(t, time.Minute, "--log-format", "json", "status", h.vmName)
	var status struct {
		Name      string `json:"name"`
		Provider  string `json:"provider"`
		Lifecycle string `json:"lifecycle"`
	}
	if err := json.Unmarshal([]byte(output), &status); err != nil {
		t.Fatalf("decode VM status: %v\n%s", err, output)
	}
	if status.Name != h.vmName || status.Provider != h.providerName || status.Lifecycle != wanted {
		t.Fatalf("unexpected VM status: name=%q provider=%q lifecycle=%q, want %q/%q/%q", status.Name, status.Provider, status.Lifecycle, h.vmName, h.providerName, wanted)
	}
}

func (h *harness) waitForLifecycle(t *testing.T, wanted string) {
	t.Helper()
	if err := h.waitForLifecycleResult(wanted); err != nil {
		t.Fatalf("timed out waiting for VM lifecycle %q: %v", wanted, err)
	}
}

func (h *harness) waitForLifecycleResult(wanted string) error {
	deadline := time.Now().Add(stopTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		output, err := h.cli(time.Minute, "--log-format", "json", "status", h.vmName)
		if err == nil {
			var status struct {
				Lifecycle string `json:"lifecycle"`
			}
			if decodeErr := json.Unmarshal([]byte(output), &status); decodeErr == nil && status.Lifecycle == wanted {
				return nil
			} else if decodeErr != nil {
				lastErr = decodeErr
			} else {
				lastErr = fmt.Errorf("lifecycle is %q", status.Lifecycle)
			}
		} else {
			lastErr = err
		}
		time.Sleep(statusPollInterval)
	}
	if lastErr == nil {
		lastErr = errors.New("no provider status was returned")
	}
	return fmt.Errorf("%v", lastErr)
}

func (h *harness) cli(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	fullArgs := make([]string, 0, len(args)+2)
	fullArgs = append(fullArgs, "--config", h.configPath)
	fullArgs = append(fullArgs, args...)
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, h.binaryPath, fullArgs...)
	cmd.Dir = filepath.Dir(h.binaryPath)
	cmd.Env = h.env
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return stdout.String(), fmt.Errorf("agent-os %s: %w\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String(), nil
}

func (h *harness) waitForHostPort(t *testing.T) {
	t.Helper()
	address := fmt.Sprintf("127.0.0.1:%d", h.port)
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp4", address, 2*time.Second)
		if err == nil {
			_ = connection.Close()
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("Orca was not reachable through the host TCP endpoint %s", address)
}

func (h *harness) assertGuestHealth(t *testing.T) {
	t.Helper()
	if err := h.execGuestRoot(guestRootHealthScript(h.distribution, h.port, credentials.GuestKeyPath(h.keyPath), h.expectedInstructions)); err != nil {
		t.Fatal(err)
	}
	if err := h.execGuestAgent(guestAgentHealthScript(h.distribution)); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) mustWriteSentinel(t *testing.T) {
	t.Helper()
	if err := h.execGuestAgent(guestSentinelWriteScript()); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) assertSentinel(t *testing.T) {
	t.Helper()
	if err := h.execGuestAgent(guestSentinelCheckScript()); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) execGuestRoot(script string) error {
	return h.execGuest(func(ctx context.Context, stdout, stderr io.Writer) error {
		return h.provider.ExecAsRoot(ctx, h.vmName, []string{"/bin/bash", "-s"}, strings.NewReader(script), stdout, stderr)
	}, "root")
}

func (h *harness) execGuestAgent(script string) error {
	return h.execGuest(func(ctx context.Context, stdout, stderr io.Writer) error {
		return h.provider.ExecAsUser(ctx, h.vmName, "agent", []string{"/bin/bash", "-s"}, strings.NewReader(script), stdout, stderr)
	}, "agent")
}

func (h *harness) execGuest(run func(context.Context, io.Writer, io.Writer) error, user string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var stdout, stderr bytes.Buffer
	if err := run(ctx, &stdout, &stderr); err != nil {
		return fmt.Errorf("guest %s health check failed: %w\nstdout:\n%s\nstderr:\n%s", user, err, stdout.String(), stderr.String())
	}
	return nil
}

func (h *harness) assertDestroyed(t *testing.T) {
	t.Helper()
	statePath := filepath.Join(h.stateDir, "v1", "vms", h.vmName)
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("VM state directory still exists after purge: %s (err=%v)", statePath, err)
	}
	profilePath := filepath.Join(h.stateDir, "v1", "profiles", h.vmName)
	if _, err := os.Stat(profilePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("profile metadata still exists after purge: %s (err=%v)", profilePath, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var output bytes.Buffer
	var stderr bytes.Buffer
	err := h.runner.Run(ctx, "limactl", []string{"list", "--format", "{{.Name}}"}, nil, &output, &stderr)
	if err != nil {
		t.Fatalf("confirm Lima VM removal: %v\nstderr:\n%s", err, stderr.String())
	}
	if hasOutputLine(output.String(), h.vmName) {
		t.Fatalf("Lima instance %q still exists after purge\n%s", h.vmName, output.String())
	}
	output.Reset()
	stderr.Reset()
	if err := h.runner.Run(ctx, "limactl", []string{"disk", "list", "--json"}, nil, &output, &stderr); err != nil {
		t.Fatalf("confirm Lima profile disk removal: %v\nstderr:\n%s", err, stderr.String())
	}
	present, err := hasLimaDiskJSON(output.String(), profile.DiskID(h.vmName))
	if err != nil {
		t.Fatalf("decode Lima profile disk list: %v\n%s", err, output.String())
	}
	if present {
		t.Fatalf("Lima profile disk %q still exists after purge\n%s", profile.DiskID(h.vmName), output.String())
	}
}

func (h *harness) printRetentionNotice(t *testing.T) {
	t.Helper()
	if h.noticePrinted {
		return
	}
	h.noticePrinted = true
	t.Logf("AGENT_OS_E2E_KEEP_VM=1 retained stopped VM %q. Inspect it with:\n%s\nCleanup with:\n%s", h.vmName, h.inspectCommand(), h.cleanupCommand())
}

func (h *harness) inspectCommand() string {
	return fmt.Sprintf("%s %s --config %s --state-dir %s status %s", h.environmentPrefix(), shellQuote(h.binaryPath), shellQuote(h.configPath), shellQuote(h.stateDir), shellQuote(h.vmName))
}

func (h *harness) cleanupCommand() string {
	return fmt.Sprintf("%s %s --config %s --state-dir %s destroy --yes --purge-profiles %s", h.environmentPrefix(), shellQuote(h.binaryPath), shellQuote(h.configPath), shellQuote(h.stateDir), shellQuote(h.vmName))
}

func (h *harness) environmentPrefix() string {
	return fmt.Sprintf("HOME=%s XDG_CONFIG_HOME=%s XDG_STATE_HOME=%s LIMA_HOME=%s", shellQuote(h.home), shellQuote(filepath.Join(h.root, "config-home")), shellQuote(filepath.Join(h.root, "state-home")), shellQuote(h.limaHome))
}

func (h *harness) cleanup() {
	if h.keepVM && h.created && !h.destroyed {
		if h.vmRunning {
			if _, err := h.cli(stopTimeout, "stop", h.vmName); err != nil {
				h.t.Errorf("cleanup operation stop %q failed; preserved E2E state at %s: %v", h.vmName, h.root, err)
				return
			}
			h.vmRunning = false
			if err := h.waitForLifecycleResult("stopped"); err != nil {
				h.t.Errorf("cleanup operation verify stopped %q failed; preserved E2E state at %s: %v", h.vmName, h.root, err)
				return
			}
		}
		if !h.noticePrinted {
			h.printRetentionNotice(h.t)
		}
		return
	}
	if h.destroyed || !h.created {
		if err := h.removeTemporaryState(); err != nil {
			h.t.Errorf("cleanup operation remove temporary state failed: %v", err)
		}
		return
	}

	if h.vmRunning {
		if _, err := h.cli(stopTimeout, "stop", h.vmName); err != nil {
			h.t.Errorf("cleanup operation stop %q failed; preserved E2E state at %s: %v", h.vmName, h.root, err)
			return
		}
		h.vmRunning = false
		if err := h.waitForLifecycleResult("stopped"); err != nil {
			h.t.Errorf("cleanup operation verify stopped %q failed; preserved E2E state at %s: %v", h.vmName, h.root, err)
			return
		}
	}
	if _, err := h.cli(e2eCommandTimeout, "destroy", "--yes", "--purge-profiles", h.vmName); err != nil {
		h.t.Errorf("cleanup operation destroy --yes --purge-profiles %q failed; preserved E2E state at %s: %v\nRun cleanup manually with:\n%s", h.vmName, h.root, err, h.cleanupCommand())
		return
	}
	h.destroyed = true
	if err := h.removeTemporaryState(); err != nil {
		h.t.Errorf("cleanup operation remove temporary state failed: %v", err)
	}
}

func (h *harness) removeTemporaryState() error {
	var errs []error
	for _, path := range []string{h.root, h.limaHome} {
		if path == "" {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", path, err))
		}
	}
	return errors.Join(errs...)
}

type isolatedRunner struct {
	env []string
}

func (r *isolatedRunner) Run(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = r.env
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("run %s: %w", name, err)
	}
	return nil
}

func buildBinary(t *testing.T, root, destination string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", destination, ".")
	cmd.Dir = root
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build current agent-os binary: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
}

func writeE2EConfig(t *testing.T, path, vmName string, port int, stateDir, keyPath string) {
	t.Helper()
	skills := provision.DefaultSkills()
	quotedSkills := make([]string, 0, len(skills))
	for _, skill := range skills {
		quotedSkills = append(quotedSkills, fmt.Sprintf("%q", skill))
	}
	contents := fmt.Sprintf(`vm:
  name: %s
  cpus: 2
  memory_mib: 4096
  disk_gib: 40
profiles:
  disk_gib: 10
access:
  mode: local
orca:
  port: %d
repository:
  key_path: %s
network:
  allowed_cidrs: []
release:
  repository: gjpin/agent-os
state:
  dir: %s
log:
  format: human
packages: []
skills: [%s]
`, yamlQuote(vmName), port, yamlQuote(keyPath), yamlQuote(stateDir), strings.Join(quotedSkills, ", "))
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write isolated configuration: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("protect isolated configuration: %v", err)
	}
}

func writeRepositoryKey(t *testing.T, privatePath string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatalf("generate E2E repository key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(private, "agent-os-e2e")
	if err != nil {
		t.Fatalf("marshal E2E repository key: %v", err)
	}
	privateBytes := pem.EncodeToMemory(block)
	publicKey, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatalf("marshal E2E repository public key: %v", err)
	}
	publicBytes := ssh.MarshalAuthorizedKey(publicKey)
	if err := os.WriteFile(privatePath, privateBytes, 0o600); err != nil {
		t.Fatalf("write E2E repository private key: %v", err)
	}
	if err := os.WriteFile(privatePath+".pub", publicBytes, 0o644); err != nil {
		t.Fatalf("write E2E repository public key: %v", err)
	}
	if err := os.Chmod(privatePath, 0o600); err != nil {
		t.Fatalf("protect E2E repository private key: %v", err)
	}
}

func isolatedEnvironment(home, configHome, stateHome, limaHome string) []string {
	blocked := make(map[string]struct{}, len(documentedAgentOSEnv)+4)
	for _, key := range documentedAgentOSEnv {
		blocked[key] = struct{}{}
	}
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "LIMA_HOME"} {
		blocked[key] = struct{}{}
	}
	env := make([]string, 0, len(os.Environ())+5)
	for _, item := range os.Environ() {
		key, _, ok := strings.Cut(item, "=")
		if ok {
			if _, skip := blocked[key]; skip {
				continue
			}
		}
		env = append(env, item)
	}
	env = append(env,
		"HOME="+home,
		"XDG_CONFIG_HOME="+configHome,
		"XDG_STATE_HOME="+stateHome,
		"LIMA_HOME="+limaHome,
	)
	sort.Strings(env)
	return env
}

func uniqueVMName(t *testing.T) string {
	t.Helper()
	var id [6]byte
	if _, err := cryptorand.Read(id[:]); err != nil {
		t.Fatalf("generate unique E2E VM name: %v", err)
	}
	return fmt.Sprintf("agent-os-e2e-%x", id)
}

func freeLocalPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate free E2E TCP port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release free E2E TCP port: %v", err)
	}
	return port
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate E2E test source")
	}
	dir := filepath.Dir(source)
	if !filepath.IsAbs(dir) {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			t.Fatalf("locate E2E package directory: %v", err)
		}
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find go.mod above %s", source)
		}
		dir = parent
	}
}

func hasOutputLine(output, wanted string) bool {
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == wanted {
			return true
		}
	}
	return false
}

func hasLimaDiskJSON(output, wanted string) (bool, error) {
	data := bytes.TrimSpace([]byte(output))
	if len(data) == 0 {
		return false, nil
	}
	type disk struct {
		Name string `json:"name"`
	}
	if data[0] == '[' {
		var disks []disk
		if err := json.Unmarshal(data, &disks); err != nil {
			return false, err
		}
		for _, value := range disks {
			if value.Name == wanted {
				return true, nil
			}
		}
		return false, nil
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var value disk
		err := decoder.Decode(&value)
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if value.Name == wanted {
			return true, nil
		}
	}
}

func yamlQuote(value string) string { return fmt.Sprintf("%q", value) }

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func guestRootHealthScript(distribution model.Distribution, orcaPort int, guestKeyPath, instructions string) string {
	var b strings.Builder
	b.WriteString("#!/bin/bash\nset -euo pipefail\n")
	b.WriteString("export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n")
	fmt.Fprintf(&b, "readonly orca_port=%s\nreadonly expected_instructions=%s\n", shellQuote(fmt.Sprint(orcaPort)), shellQuote(instructions))
	if distribution == model.DistributionDebian {
		b.WriteString("dpkg-query --show --")
	} else {
		b.WriteString("rpm -q --")
	}
	for _, packageName := range provision.BaselinePackages(distribution) {
		fmt.Fprintf(&b, " %s", shellQuote(packageName))
	}
	b.WriteString("\n")
	b.WriteString(`test -x /usr/bin/orca
test -f /etc/agent-os/firewall.rules
test -f /etc/systemd/system/orca.service
test -f /etc/systemd/system/agent-os-firewall.service
systemctl is-enabled --quiet orca.service
systemctl is-active --quiet orca.service
systemctl is-enabled --quiet agent-os-firewall.service
systemctl is-active --quiet agent-os-firewall.service
mount_fstype="$(findmnt -no FSTYPE --target /var/lib/agent-os/profile)"
test "$mount_fstype" = ext4
mount_options="$(findmnt -no OPTIONS --target /var/lib/agent-os/profile)"
case ",$mount_options," in
  *,nodev,*) ;;
  *) echo "profile mount is missing nodev: $mount_info" >&2; exit 1 ;;
esac
case ",$mount_options," in
  *,nosuid,*) ;;
  *) echo "profile mount is missing nosuid: $mount_info" >&2; exit 1 ;;
esac
`)
	for _, route := range profileRoutes() {
		fmt.Fprintf(&b, "test -L %s\ntest \"$(readlink -- %s)\" = %s\n", shellQuote(route.path), shellQuote(route.path), shellQuote(route.target))
	}
	for _, link := range instructionLinks() {
		fmt.Fprintf(&b, "test -L %s\ntest \"$(readlink -- %s)\" = %s\n", shellQuote(link.path), shellQuote(link.path), shellQuote(link.target))
	}
	fmt.Fprintf(&b, `printf '%%s' "$expected_instructions" | cmp -s - %s
test -f %s
test "$(stat -c '%%a' %s)" = 600
test "$(stat -c '%%U:%%G' %s)" = root:root
test -f /home/agent/.codex/config.toml
grep -Eq '^[[:space:]]*cli_auth_credentials_store[[:space:]]*=[[:space:]]*"file"[[:space:]]*$' /home/agent/.codex/config.toml
grep -Fq 'tcp dport %d' /etc/agent-os/firewall.rules
firewall_rules="$(nft list table inet agent_os)"
printf '%%s\n' "$firewall_rules" | grep -Fq 'policy drop'
printf '%%s\n' "$firewall_rules" | grep -Fq "tcp dport %d"
if ! ss -H -ltn | awk -v port=":%d" '$4 ~ (port "$") { found=1 } END { exit !found }'; then
  ss -H -ltn
  exit 1
fi
if [ -e /home/agent/.claude.json ] || [ -L /home/agent/.claude.json ]; then
  test -f /home/agent/.claude.json
  test ! -L /home/agent/.claude.json
  test -f /var/lib/agent-os/profile/claude.json
  cmp -s /home/agent/.claude.json /var/lib/agent-os/profile/claude.json
fi
`, shellQuote(instructionsPath()), shellQuote(guestKeyPath), shellQuote(guestKeyPath), shellQuote(guestKeyPath), orcaPort, orcaPort, orcaPort)
	if distribution == model.DistributionDebian {
		b.WriteString(`dpkg-query --show -- google-chrome-stable docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
! dpkg-query --show -- podman podman-docker >/dev/null 2>&1
! command -v podman >/dev/null 2>&1
systemctl is-enabled --quiet docker.service
systemctl is-active --quiet docker.service
id -nG agent | tr ' ' '\n' | grep -Fxq docker
test -x /usr/bin/kind
`)
	} else {
		b.WriteString(`rpm -q -- google-chrome-stable podman podman-docker buildah skopeo
agent_uid="$(id -u agent)"
user_unit_state="$(systemctl is-enabled "user@$agent_uid.service" 2>/dev/null || true)"
case "$user_unit_state" in
  enabled|static|indirect|generated|alias) ;;
  *) echo "user@$agent_uid.service is not enabled: $user_unit_state" >&2; exit 1 ;;
esac
systemctl is-active --quiet "user@$agent_uid.service"
delegate="$(systemctl show --property=Delegate --value "user@$agent_uid.service")"
printf '%s\n' "$delegate" | grep -Eiq '^(yes|true)$'
linger="$(loginctl show-user agent --property=Linger --value)"
printf '%s\n' "$linger" | grep -Eiq '^(yes|true)$'
test "$(stat -fc %T /sys/fs/cgroup)" = cgroup2fs
test -x /usr/local/bin/kind
grep -Fq 'KIND_EXPERIMENTAL_PROVIDER' /usr/local/bin/kind
grep -Fq 'log_driver = "k8s-file"' /home/agent/.config/containers/containers.conf.d/agent-os-kind.conf
grep -Fq 'pids_limit = 65536' /home/agent/.config/containers/containers.conf.d/agent-os-kind.conf
`)
	}
	b.WriteString("test -f /home/agent/.agent-os/AGENTS.md\n")
	return b.String()
}

func guestAgentHealthScript(distribution model.Distribution) string {
	var b strings.Builder
	b.WriteString(`#!/bin/bash
set -euo pipefail
test "$(id -un)" = agent
test "$(id -u)" -ne 0
test "$HOME" = /home/agent
test "$PATH" = '/home/agent/.local/bin:/home/agent/.opencode/bin:/usr/local/bin:/usr/bin:/bin'
test "$CODEX_HOME" = /home/agent/.codex
test "$COPILOT_HOME" = /home/agent/.copilot
`)
	for _, executable := range guestExecutables(distribution) {
		fmt.Fprintf(&b, "resolved=$(command -v %s)\ntest -x \"$resolved\"\n", shellQuote(executable))
	}
	b.WriteString(`test -f "$CODEX_HOME/config.toml"
grep -Eq '^[[:space:]]*cli_auth_credentials_store[[:space:]]*=[[:space:]]*"file"[[:space:]]*$' "$CODEX_HOME/config.toml"
test -n "$(find "$HOME/.agents/skills" -type f -name SKILL.md -print -quit)"
`)
	if distribution == model.DistributionDebian {
		b.WriteString("docker info >/dev/null\n")
	} else {
		b.WriteString(`export XDG_RUNTIME_DIR="/run/user/$(id -u)"
export DBUS_SESSION_BUS_ADDRESS="unix:path=$XDG_RUNTIME_DIR/bus"
podman info --format json | jq -e '(.host.security.rootless == true) and (.host.cgroupVersion == "v2" or .host.cgroupVersion == "2")' >/dev/null
`)
	}
	return b.String()
}

func guestSentinelWriteScript() string {
	return `#!/bin/bash
set -euo pipefail
sentinel='agent-os-e2e-profile-sentinel-v1'
printf '%s\n' "$sentinel" > "$HOME/.agent-os/e2e-sentinel"
printf '%s\n' '{"agent_os_e2e":"sentinel"}' > "$HOME/.claude.json"
chmod 0600 "$HOME/.claude.json"
test "$(cat "$HOME/.agent-os/e2e-sentinel")" = "$sentinel"
test "$(cat /var/lib/agent-os/profile/agent-os/e2e-sentinel)" = "$sentinel"
`
}

func guestSentinelCheckScript() string {
	return `#!/bin/bash
set -euo pipefail
sentinel='agent-os-e2e-profile-sentinel-v1'
test -L "$HOME/.agent-os"
test -f "$HOME/.agent-os/e2e-sentinel"
test "$(cat "$HOME/.agent-os/e2e-sentinel")" = "$sentinel"
test "$(cat /var/lib/agent-os/profile/agent-os/e2e-sentinel)" = "$sentinel"
test -f "$HOME/.claude.json"
test ! -L "$HOME/.claude.json"
test "$(cat "$HOME/.claude.json")" = '{"agent_os_e2e":"sentinel"}'
test "$(cat /var/lib/agent-os/profile/claude.json)" = '{"agent_os_e2e":"sentinel"}'
`
}

type guestRoute struct {
	path   string
	target string
}

func profileRoutes() []guestRoute {
	return []guestRoute{
		{path: "/home/agent/.config/opencode", target: "/var/lib/agent-os/profile/opencode/config"},
		{path: "/home/agent/.config/orca", target: "/var/lib/agent-os/profile/orca"},
		{path: "/home/agent/.local/share/opencode", target: "/var/lib/agent-os/profile/opencode/data"},
		{path: "/home/agent/.codex", target: "/var/lib/agent-os/profile/codex"},
		{path: "/home/agent/.claude", target: "/var/lib/agent-os/profile/claude"},
		{path: "/home/agent/.pi/agent", target: "/var/lib/agent-os/profile/pi-agent"},
		{path: "/home/agent/.copilot", target: "/var/lib/agent-os/profile/copilot"},
		{path: "/home/agent/.agents", target: "/var/lib/agent-os/profile/agents"},
		{path: "/home/agent/.agent-os", target: "/var/lib/agent-os/profile/agent-os"},
	}
}

func instructionLinks() []guestRoute {
	return []guestRoute{
		{path: provision.AgentInstructionsOpencodePath, target: provision.AgentInstructionsCanonicalPath},
		{path: provision.AgentInstructionsCodexPath, target: provision.AgentInstructionsCanonicalPath},
		{path: provision.AgentInstructionsClaudePath, target: provision.AgentInstructionsCanonicalPath},
		{path: provision.AgentInstructionsPiPath, target: provision.AgentInstructionsCanonicalPath},
		{path: provision.AgentInstructionsCopilotPath, target: provision.AgentInstructionsCanonicalPath},
	}
}

func instructionsPath() string { return provision.AgentInstructionsCanonicalPath }

func guestExecutables(distribution model.Distribution) []string {
	executables := provision.RequiredExecutables(provision.BaselinePackages(distribution))
	executables = append(executables, []string{
		"node", "npm", "pnpm", "terraform", "tofu", "helm", "uv", "uvx", "java", "javac",
		"kind", "opencode", "codex", "claude", "agy", "pi", "copilot", "devcontainer",
		"google-chrome-stable", "chrome-devtools", "chrome-devtools-mcp",
	}...)
	if distribution == model.DistributionDebian {
		executables = append(executables, "docker")
	} else {
		executables = append(executables, "podman", "buildah", "skopeo")
	}
	seen := make(map[string]struct{}, len(executables))
	result := make([]string, 0, len(executables))
	for _, executable := range executables {
		if _, ok := seen[executable]; ok {
			continue
		}
		seen[executable] = struct{}{}
		result = append(result, executable)
	}
	sort.Strings(result)
	return result
}

// Keep the compile-time dependency on the provider interface visible to the
// test package even if a future provider adds a different executor shape.
var _ execx.Runner = (*isolatedRunner)(nil)
