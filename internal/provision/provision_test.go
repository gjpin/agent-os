package provision

import (
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestBaselinePackagesAreExactSortedAndDuplicateFree(t *testing.T) {
	want := strings.Fields(`
bash bat bats bind-utils bzip2 ca-certificates cargo chromium
clang clang-tools-extra cmake coreutils curl diffutils fd-find file
findutils fzf gawk gcc gcc-c++ gdb gettext-envsubst gh git git-lfs
golang gopls grep gzip iproute iputils jq just less lld lldb llvm lsof
make meson mold ninja-build nmap-ncat
nodejs24 nodejs24-bin nodejs24-npm nodejs24-npm-bin
openssh-clients openssl patch pkgconf-pkg-config procps-ng
python3 python3-devel python3-pytest-xdist ripgrep rsync rust
rust-analyzer rustfmt scons sed ShellCheck shfmt sqlite strace tar time
tree tree-sitter-cli unzip util-linux uv wget which xxd xz yq zip zstd
pnpm vim-enhanced
podman podman-docker buildah skopeo
dnf-plugins-core rpm-build rpmdevtools redhat-rpm-config
autoconf automake libtool m4 ccache
valgrind perf psmisc sysstat
socat tcpdump traceroute
acl attr entr inotify-tools
python3-pip pipx
direnv tmux
moreutils parallel gnupg2 man-db man-pages
kubernetes1.36-client helm kind kustomize opentofu
`)
	sort.Strings(want)
	got := BaselinePackages()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("baseline package set differs:\n got: %v\nwant: %v", got, want)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("baseline is not sorted and duplicate-free at %q, %q", got[i-1], got[i])
		}
	}
}

func TestPackageManifestMergesAdditionsDeterministically(t *testing.T) {
	got, err := PackageManifest([]string{"htop", "git", "htop"})
	if err != nil {
		t.Fatal(err)
	}
	if !sort.StringsAreSorted(got) {
		t.Fatalf("manifest is not sorted: %v", got)
	}
	counts := make(map[string]int, len(got))
	for _, pkg := range got {
		counts[pkg]++
	}
	for _, pkg := range BaselinePackages() {
		if counts[pkg] != 1 {
			t.Errorf("baseline package %q occurs %d times", pkg, counts[pkg])
		}
	}
	if counts["htop"] != 1 {
		t.Fatalf("custom addition occurs %d times", counts["htop"])
	}
	if _, err := PackageManifest([]string{"valid", "--invalid"}); err == nil {
		t.Fatal("manifest accepted an invalid operator addition")
	}
}

func TestAgentInstructionsScriptContainsAllManagedDestinations(t *testing.T) {
	content := "instructions '$HOME'\nwith a trailing newline\n"
	script := AgentInstructionsScript(content)
	paths := []string{
		AgentInstructionsCanonicalPath,
		AgentInstructionsOpencodePath,
		AgentInstructionsCodexPath,
		AgentInstructionsClaudePath,
		AgentInstructionsPiPath,
		AgentInstructionsCopilotPath,
	}
	for _, destination := range paths {
		if !strings.Contains(script, "rm -rf -- "+shellQuote(destination)) {
			t.Errorf("script does not force-remove %s", destination)
		}
	}
	for _, link := range paths[1:] {
		if !strings.Contains(script, "ln -s -- "+shellQuote(paths[0])+
			" "+shellQuote(link)) {
			t.Errorf("script does not create absolute link %s", link)
		}
	}
	if !strings.Contains(script, "printf '%s' "+shellQuote(content)) {
		t.Fatal("script does not contain the exact embedded content")
	}
	if !strings.Contains(script, "install -d -o 'agent' -g 'agent' -m 0755 '/home/agent/.agent-os'") {
		t.Fatal("script does not create the canonical directory as agent")
	}
}

func TestAgentInstructionsScriptIsIdempotentAndReplacesDestinations(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home", "agent")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "embedded instructions\n"
	owner := strconv.Itoa(os.Getuid())
	group := strconv.Itoa(os.Getgid())
	script := agentInstructionsScriptAt(content, home, owner, group)
	scriptPath := filepath.Join(t.TempDir(), "provision.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	destinations := []string{
		path.Join(home, ".agent-os", "AGENTS.md"),
		path.Join(home, ".config", "opencode", "AGENTS.md"),
		path.Join(home, ".codex", "AGENTS.md"),
		path.Join(home, ".claude", "CLAUDE.md"),
		path.Join(home, ".pi", "agent", "AGENTS.md"),
		path.Join(home, ".copilot", "copilot-instructions.md"),
	}
	for i, destination := range destinations {
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			t.Fatal(err)
		}
		switch i % 3 {
		case 0:
			if err := os.Mkdir(destination, 0o755); err != nil {
				t.Fatal(err)
			}
		case 1:
			if err := os.WriteFile(destination, []byte("stale"), 0o600); err != nil {
				t.Fatal(err)
			}
		default:
			if err := os.Symlink("/tmp/stale-agent-os-link", destination); err != nil {
				t.Fatal(err)
			}
		}
	}

	for run := 0; run < 2; run++ {
		command := exec.Command("bash", scriptPath)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("provisioning run %d failed: %v\n%s", run+1, err, output)
		}
	}

	canonical := destinations[0]
	actual, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if string(actual) != content {
		t.Fatalf("canonical content = %q, want %q", actual, content)
	}
	for _, destination := range destinations[1:] {
		info, err := os.Lstat(destination)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s is not a symlink", destination)
		}
		target, err := os.Readlink(destination)
		if err != nil {
			t.Fatal(err)
		}
		if target != canonical {
			t.Fatalf("%s points to %q, want %q", destination, target, canonical)
		}
	}
}

func TestCodingAgentsScriptContainsRequiredInstallersAndSafetyControls(t *testing.T) {
	script := CodingAgentsScript()
	command := exec.Command("bash", "-n")
	command.Stdin = strings.NewReader(script)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("coding-agent installer is not valid Bash: %v\n%s", err, output)
	}
	for _, expected := range []string{
		"set -euo pipefail",
		"dnf install -y -- dnf-plugins-core",
		"dnf config-manager addrepo --from-repofile=https://rpm.releases.hashicorp.com/fedora/hashicorp.repo",
		"dnf install -y -- terraform",
		"dnf install -y -- adoptium-temurin-java-repository",
		"dnf config-manager setopt adoptium.enabled=1",
		"dnf install -y -- temurin-25-jdk",
		"for executable in node npm terraform",
		"https://opencode.ai/install",
		"https://chatgpt.com/codex/install.sh",
		"https://claude.ai/install.sh",
		"@earendil-works/pi-coding-agent",
		"https://gh.io/copilot-install",
		"--retry 5 --retry-delay 2 --retry-all-errors",
		"mktemp -d /tmp/agent-os-coding-agents.XXXXXX",
		"trap cleanup EXIT",
		"/usr/sbin/runuser --user agent -- /usr/bin/env",
		"HOME=\"$agent_home\" SHELL=/bin/bash PATH=\"$managed_path\"",
		"--no-modify-path",
		"--global --ignore-scripts",
		"command -v \"$1\"",
		"grep -Fqx \"$path_line\"",
		"touch \"$ready_marker\"",
	} {
		if !strings.Contains(script, expected) {
			t.Errorf("coding-agent installer omits %q", expected)
		}
	}
	if strings.Index(script, "if [ -f \"$ready_marker\" ]") > strings.Index(script, "dnf install") {
		t.Fatal("idempotency guard runs after package installation")
	}
	pluginInstall := strings.Index(script, "dnf install -y -- dnf-plugins-core")
	hashicorpRepository := strings.Index(script, "dnf config-manager addrepo --from-repofile=https://rpm.releases.hashicorp.com/fedora/hashicorp.repo")
	terraformInstall := strings.Index(script, "dnf install -y -- terraform")
	repositoryInstall := strings.Index(script, "dnf install -y -- adoptium-temurin-java-repository")
	repositoryEnable := strings.Index(script, "dnf config-manager setopt adoptium.enabled=1")
	jdkInstall := strings.Index(script, "dnf install -y -- temurin-25-jdk")
	nodeValidation := strings.Index(script, "for executable in node npm")
	npmInstall := strings.Index(script, "/usr/bin/npm install")
	if !(pluginInstall < hashicorpRepository && hashicorpRepository < terraformInstall && terraformInstall < repositoryInstall && repositoryInstall < repositoryEnable && repositoryEnable < jdkInstall && jdkInstall < nodeValidation && nodeValidation < npmInstall) {
		t.Fatalf("provisioning commands are out of order: plugin install=%d HashiCorp repository=%d Terraform install=%d repository install=%d enable=%d JDK install=%d executable validation=%d npm install=%d", pluginInstall, hashicorpRepository, terraformInstall, repositoryInstall, repositoryEnable, jdkInstall, nodeValidation, npmInstall)
	}
	if strings.Contains(script, "nodejs26") || strings.Contains(script, "dnf install -y -- nodejs") {
		t.Fatal("coding-agent bootstrap installs Node instead of using the baseline")
	}
	javaValidation := strings.Index(script, "for executable in java javac")
	if javaValidation < jdkInstall {
		t.Fatal("Java executables are validated before the JDK is installed")
	}
	if strings.Index(script, "touch \"$ready_marker\"") < javaValidation {
		t.Fatal("readiness marker is written before java and javac validation")
	}
	if strings.Index(script, "touch \"$ready_marker\"") < strings.Index(script, "for executable in") {
		t.Fatal("readiness marker is written before executable validation")
	}
	if strings.Count(script, "readonly path_line=") != 1 {
		t.Fatal("managed PATH line is not defined exactly once")
	}
}

func TestKindPodmanScriptContainsRootlessPrerequisites(t *testing.T) {
	script := KindPodmanScript()
	command := exec.Command("bash", "-n")
	command.Stdin = strings.NewReader(script)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("kind Podman setup is not valid Bash: %v\n%s", err, output)
	}
	for _, expected := range []string{
		"set -euo pipefail",
		"$agent_home/.config/containers/containers.conf.d/agent-os-kind.conf",
		`log_driver = "k8s-file"`,
		"pids_limit = 65536",
		"ip6_tables\nip6table_nat\nip_tables\niptable_nat",
		`modprobe "$module"`,
		"fs.inotify.max_user_watches = 524288",
		"fs.inotify.max_user_instances = 512",
		"sysctl --system",
		"/etc/systemd/system/user@.service.d/agent-os-kind.conf",
		"Delegate=yes",
		`loginctl enable-linger "$agent_user"`,
		`systemctl start "user@$agent_uid.service"`,
		`export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$user_uid}"`,
		`export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=$XDG_RUNTIME_DIR/bus}"`,
		`export KIND_EXPERIMENTAL_PROVIDER="${KIND_EXPERIMENTAL_PROVIDER:-podman}"`,
		"/usr/bin/systemd-run --user --scope --quiet",
		"--property=Delegate=yes -- /usr/bin/kind \"$@\"",
		`test "$(stat -fc %T /sys/fs/cgroup)" = cgroup2fs`,
		"/usr/bin/kind /usr/bin/podman",
	} {
		if !strings.Contains(script, expected) {
			t.Errorf("kind Podman setup omits %q", expected)
		}
	}
	if strings.Contains(script, "net.ipv4.ip_unprivileged_port_start") || strings.Contains(script, "kernel.dmesg_restrict") {
		t.Fatal("kind Podman setup relaxes an excluded sysctl")
	}
}
