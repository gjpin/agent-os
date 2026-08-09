package provision

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

const (
	AgentInstructionsCanonicalPath = "/home/agent/.agent-os/AGENTS.md"
	AgentInstructionsOpencodePath  = "/home/agent/.config/opencode/AGENTS.md"
	AgentInstructionsCodexPath     = "/home/agent/.codex/AGENTS.md"
	AgentInstructionsClaudePath    = "/home/agent/.claude/CLAUDE.md"
	AgentInstructionsPiPath        = "/home/agent/.pi/agent/AGENTS.md"
	AgentInstructionsCopilotPath   = "/home/agent/.copilot/copilot-instructions.md"
	CodingAgentsReadyPath          = "/var/lib/agent-os/coding-agents-ready"
	AgentHome                      = "/home/agent"
	AgentManagedPath               = "/home/agent/.local/bin:/home/agent/.opencode/bin:/usr/local/bin:/usr/bin:/bin"
)

// baselinePackages is the development and operations toolset guaranteed in
// every newly provisioned VM. Provider-specific packages are added by the
// provider artifact generator, while configured packages are additive extras.
var baselinePackages = strings.Fields(`
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
kubernetes1.36-client helm kustomize opentofu
`)

// BaselinePackages returns a sorted copy of the packages guaranteed in every
// VM. Callers cannot mutate the canonical manifest.
func BaselinePackages() []string {
	return mergePackageSets(baselinePackages)
}

// PackageManifest validates operator additions and merges them into the
// guaranteed baseline. Duplicate additions, including baseline packages, are
// harmless and the resulting manifest is always sorted and duplicate-free.
func PackageManifest(additions []string) ([]string, error) {
	if err := ValidatePackages(additions); err != nil {
		return nil, err
	}
	return mergePackageSets(baselinePackages, additions), nil
}

func mergePackageSets(sets ...[]string) []string {
	seen := make(map[string]struct{})
	for _, set := range sets {
		for _, pkg := range set {
			seen[pkg] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for pkg := range seen {
		result = append(result, pkg)
	}
	sort.Strings(result)
	return result
}

// CodingAgentsScript returns the root-run, idempotent installer for the five
// coding agents included in every VM. Upstream installers intentionally select
// their latest release at first boot; the marker prevents a later provisioning
// replay from unexpectedly upgrading an existing VM.
func CodingAgentsScript() string {
	return `#!/bin/bash
set -euo pipefail

readonly agent_home=/home/agent
readonly ready_marker=/var/lib/agent-os/coding-agents-ready
readonly managed_path=/home/agent/.local/bin:/home/agent/.opencode/bin:/usr/local/bin:/usr/bin:/bin

if [ -f "$ready_marker" ]; then
  exit 0
fi

dnf install -y -- adoptium-temurin-java-repository
dnf config-manager setopt adoptium.enabled=1
dnf install -y -- temurin-25-jdk

for executable in node npm; do
  resolved=$(command -v "$executable")
  test -x "$resolved"
done
install -d -o agent -g agent -m 0755 "$agent_home/.local/bin" "$agent_home/.opencode/bin"

installer_dir=$(mktemp -d /tmp/agent-os-coding-agents.XXXXXX)
cleanup() {
  rm -rf -- "$installer_dir"
}
trap cleanup EXIT
chmod 0755 "$installer_dir"

download_installer() {
  local url=$1
  local destination=$2
  curl --fail --silent --show-error --location \
    --retry 5 --retry-delay 2 --retry-all-errors \
    --output "$destination" "$url"
  chmod 0644 "$destination"
}

run_as_agent() {
  /usr/sbin/runuser --user agent -- /usr/bin/env \
    HOME="$agent_home" SHELL=/bin/bash PATH="$managed_path" "$@"
}

download_installer https://opencode.ai/install "$installer_dir/opencode.sh"
run_as_agent /bin/bash "$installer_dir/opencode.sh" --no-modify-path

download_installer https://chatgpt.com/codex/install.sh "$installer_dir/codex.sh"
run_as_agent /usr/bin/env CODEX_INSTALL_DIR="$agent_home/.local/bin" \
  /bin/sh "$installer_dir/codex.sh"

download_installer https://claude.ai/install.sh "$installer_dir/claude.sh"
run_as_agent /bin/bash "$installer_dir/claude.sh" latest

run_as_agent /usr/bin/npm install --global --ignore-scripts \
  --prefix "$agent_home/.local" @earendil-works/pi-coding-agent

download_installer https://gh.io/copilot-install "$installer_dir/copilot.sh"
run_as_agent /usr/bin/env PREFIX="$agent_home/.local" \
  /bin/bash "$installer_dir/copilot.sh"

for executable in java javac; do
  resolved=$(command -v "$executable")
  test -x "$resolved"
done

for executable in opencode codex claude pi copilot; do
  run_as_agent /bin/sh -c 'resolved=$(command -v "$1") && test -x "$resolved"' sh "$executable"
done

readonly path_line='export PATH="$HOME/.local/bin:$HOME/.opencode/bin:$PATH"'
for profile in "$agent_home/.bash_profile" "$agent_home/.bashrc"; do
  touch "$profile"
  chown agent:agent "$profile"
  if ! grep -Fqx "$path_line" "$profile"; then
    printf '%s\n' "$path_line" >> "$profile"
  fi
done

install -d -o root -g root -m 0755 /var/lib/agent-os
touch "$ready_marker"
chmod 0644 "$ready_marker"
`
}

// AgentInstructionsScript returns a root-run, idempotent provisioning script
// for the repository instructions. The content is shell-quoted so its bytes,
// including a trailing newline, are written without interpreting it as shell
// syntax.
func AgentInstructionsScript(content string) string {
	return agentInstructionsScriptAt(content, "/home/agent", "agent", "agent")
}

func agentInstructionsScriptAt(content, home, user, group string) string {
	canonical := path.Join(home, ".agent-os", "AGENTS.md")
	links := []string{
		path.Join(home, ".config", "opencode", "AGENTS.md"),
		path.Join(home, ".codex", "AGENTS.md"),
		path.Join(home, ".claude", "CLAUDE.md"),
		path.Join(home, ".pi", "agent", "AGENTS.md"),
		path.Join(home, ".copilot", "copilot-instructions.md"),
	}

	var b strings.Builder
	b.WriteString("#!/bin/bash\nset -eu\n\n")
	fmt.Fprintf(&b, "rm -rf -- %s\n", shellQuote(canonical))
	fmt.Fprintf(&b, "install -d -o %s -g %s -m 0755 %s\n", shellQuote(user), shellQuote(group), shellQuote(path.Dir(canonical)))
	fmt.Fprintf(&b, "printf '%%s' %s > %s\n", shellQuote(content), shellQuote(canonical))
	fmt.Fprintf(&b, "chown %s:%s %s\nchmod 0644 %s\n\n", shellQuote(user), shellQuote(group), shellQuote(canonical), shellQuote(canonical))
	for _, link := range links {
		fmt.Fprintf(&b, "rm -rf -- %s\n", shellQuote(link))
		fmt.Fprintf(&b, "install -d -o %s -g %s -m 0755 %s\n", shellQuote(user), shellQuote(group), shellQuote(path.Dir(link)))
		fmt.Fprintf(&b, "ln -s -- %s %s\n\n", shellQuote(canonical), shellQuote(link))
	}
	return b.String()
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

var packageName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+_.:@=-]*$`)

var executableMap = map[string]string{
	"git":          "git",
	"curl":         "curl",
	"jq":           "jq",
	"ripgrep":      "rg",
	"fd-find":      "fd",
	"tmux":         "tmux",
	"vim-enhanced": "vim",
}

func ValidatePackages(packages []string) error {
	for _, pkg := range packages {
		if !packageName.MatchString(pkg) || strings.Contains(pkg, "--") {
			return fmt.Errorf("invalid package name %q", pkg)
		}
	}
	return nil
}

func RequiredExecutables(packages []string) []string {
	result := make([]string, 0, len(packages))
	for _, pkg := range packages {
		if executable, ok := executableMap[pkg]; ok {
			result = append(result, executable)
		}
	}
	sort.Strings(result)
	return result
}

func InstallCommand(packages []string) []string {
	result := []string{"dnf", "install", "-y", "--"}
	result = append(result, packages...)
	return result
}

func AgentUserCloudInit() string {
	return "name: agent\n  shell: /bin/bash\n  lock_passwd: true\n  groups: []\n  sudo: []\n"
}
