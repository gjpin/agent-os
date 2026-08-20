package provision

import (
	"fmt"
	"net/url"
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
	ProvisioningReadyPath          = "/var/lib/agent-os/provision-ready"
	OrcaSkillsReadyPath            = "/var/lib/agent-os/orca-skills-ready"
	AgentHome                      = "/home/agent"
	AgentManagedPath               = "/home/agent/.local/bin:/home/agent/.opencode/bin:/usr/local/bin:/usr/bin:/bin"
	ProfileMountPath               = "/var/lib/agent-os/profile"
	ProfileSyncPath                = "/usr/local/libexec/agent-os-profile-sync"
	ProfileSetupPath               = "/usr/local/libexec/agent-os-profile-setup"
	ProfileRestoreServicePath      = "/etc/systemd/system/agent-os-profile-restore.service"
	DefaultChromeDevToolsSkillURL  = "https://github.com/ChromeDevTools/chrome-devtools-mcp/tree/main/skills/chrome-devtools-cli"
)

// Distribution identifies the guest operating system selected at VM creation.
type Distribution string

const (
	DistributionFedora Distribution = "fedora"
	DistributionDebian Distribution = "debian"
)

func (d Distribution) Valid() bool {
	return d == DistributionFedora || d == DistributionDebian
}

func ParseDistribution(value string) (Distribution, error) {
	distribution := Distribution(strings.ToLower(strings.TrimSpace(value)))
	if !distribution.Valid() {
		return "", fmt.Errorf("unsupported distro %q; choose fedora or debian", value)
	}
	return distribution, nil
}

var defaultSkills = []string{DefaultChromeDevToolsSkillURL}

// DefaultSkills returns a copy of the skills installed in every new VM.
func DefaultSkills() []string { return append([]string(nil), defaultSkills...) }

// MergeSkills keeps the built-in skill and returns a stable duplicate-free
// list without removing unrelated skills already present in the guest profile.
func MergeSkills(additions []string) []string {
	seen := make(map[string]struct{}, len(defaultSkills)+len(additions))
	result := make([]string, 0, len(defaultSkills)+len(additions))
	all := make([]string, 0, len(defaultSkills)+len(additions))
	all = append(all, defaultSkills...)
	all = append(all, additions...)
	for _, skill := range all {
		if _, ok := seen[skill]; ok {
			continue
		}
		seen[skill] = struct{}{}
		result = append(result, skill)
	}
	return result
}

// ValidateSkills accepts only public GitHub tree URLs. Restricting the source
// form keeps provisioning deterministic and prevents arbitrary URL schemes or
// hosts from becoming guest bootstrap commands.
func ValidateSkills(skills []string) error {
	for _, skill := range skills {
		if err := validateSkillURL(skill); err != nil {
			return err
		}
	}
	return nil
}

func SkillManifest(additions []string) ([]string, error) {
	if err := ValidateSkills(additions); err != nil {
		return nil, err
	}
	return MergeSkills(additions), nil
}

func renderSkillInstallCommands(skills []string) string {
	var b strings.Builder
	for _, skill := range skills {
		fmt.Fprintf(&b, "run_as_agent skills add %s --global --copy --agent cline --yes\n", shellQuote(skill))
	}
	return b.String()
}

// ChromeDevToolsScript returns the root-run, repeatable guest-local installer
// for Chrome DevTools MCP and the configured skills. It deliberately runs the
// npm and skill installers as agent so their files are owned by the user and
// land on the managed guest PATH/profile.
func ChromeDevToolsScript(configuredSkills ...[]string) string {
	skills := DefaultSkills()
	if len(configuredSkills) > 0 {
		manifest, err := SkillManifest(configuredSkills[0])
		if err != nil {
			return invalidProvisionScript(err)
		}
		skills = manifest
	}
	return `#!/bin/bash
set -euo pipefail

readonly agent_home=/home/agent
readonly managed_path=/home/agent/.local/bin:/home/agent/.opencode/bin:/usr/local/bin:/usr/bin:/bin

cd "$agent_home"

install -d -o agent -g agent -m 0755 "$agent_home/.local" "$agent_home/.local/bin" "$agent_home/.agents/skills"

run_as_agent() {
  /usr/sbin/runuser --user agent -- /usr/bin/env \
    HOME="$agent_home" SHELL=/bin/bash PATH="$managed_path" \
    CODEX_HOME="$agent_home/.codex" COPILOT_HOME="$agent_home/.copilot" "$@"
}

run_as_agent /usr/bin/npm install --global --ignore-scripts \
  --prefix "$agent_home/.local" chrome-devtools-mcp@latest skills@latest
` + renderSkillInstallCommands(skills) + `
for executable in chrome-devtools chrome-devtools-mcp; do
  run_as_agent /bin/sh -c 'resolved=$(command -v "$1") && test -x "$resolved"' sh "$executable"
done
run_as_agent /bin/sh -c 'test -n "$(find "$HOME/.agents/skills" -name SKILL.md -print -quit)"'
`
}

// OrcaSkillsScript returns the root-run, repeatable installer for the shared
// Orca skills that are available to every coding agent in a new VM.
func OrcaSkillsScript() string {
	return `#!/bin/bash
set -euo pipefail

readonly agent_home=/home/agent
readonly ready_marker=/var/lib/agent-os/orca-skills-ready
readonly managed_path=/home/agent/.local/bin:/home/agent/.opencode/bin:/usr/local/bin:/usr/bin:/bin

if [ -f "$ready_marker" ]; then
  exit 0
fi

install -d -o agent -g agent -m 0755 "$agent_home/.agents/skills"

run_as_agent() {
  /usr/sbin/runuser --user agent -- /usr/bin/env \
    HOME="$agent_home" SHELL=/bin/bash PATH="$managed_path" \
    CODEX_HOME="$agent_home/.codex" COPILOT_HOME="$agent_home/.copilot" "$@"
}

run_as_agent /usr/bin/orca skills install \
  --skill orca-cli \
  --skill orchestration \
  --agent universal
run_as_agent /usr/bin/orca skills update --all

install -d -o root -g root -m 0755 /var/lib/agent-os
touch "$ready_marker"
chmod 0644 "$ready_marker"
`
}

func invalidProvisionScript(err error) string {
	return "#!/bin/bash\nset -eu\necho " + shellQuote(err.Error()) + " >&2\nexit 1\n"
}

func chromeDevToolsInstallBlock(skills []string) string {
	return `
run_as_agent /usr/bin/npm install --global --ignore-scripts \
  --prefix "$agent_home/.local" chrome-devtools-mcp@latest skills@latest
` + renderSkillInstallCommands(skills) + `
for executable in chrome-devtools chrome-devtools-mcp; do
  run_as_agent /bin/sh -c 'resolved=$(command -v "$1") && test -x "$resolved"' sh "$executable"
done
run_as_agent /bin/sh -c 'test -n "$(find "$HOME/.agents/skills" -name SKILL.md -print -quit)"'
`
}

func validateSkillURL(value string) error {
	if strings.TrimSpace(value) != value || value == "" || len(value) > 2048 {
		return fmt.Errorf("invalid skill URL %q: must be an HTTPS GitHub tree URL", value)
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("invalid skill URL %q: must be an HTTPS GitHub tree URL", value)
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 5 || parts[0] == "" || parts[1] == "" || parts[2] != "tree" || parts[3] == "" || parts[len(parts)-1] == "" {
		return fmt.Errorf("invalid skill URL %q: must be an HTTPS GitHub tree URL", value)
	}
	for _, part := range parts {
		if part == "." || part == ".." || strings.ContainsAny(part, "\r\n\t") {
			return fmt.Errorf("invalid skill URL %q: must be an HTTPS GitHub tree URL", value)
		}
	}
	return nil
}

// ProfileMountSpec describes the Lima-managed profile disk without putting
// host paths or credentials into guest configuration.
type ProfileMountSpec struct {
	DiskID string
	Label  string
}

// fedoraBaselinePackages is the canonical development and operations toolset.
// Debian derives the same capabilities through explicit package replacements.
var fedoraBaselinePackages = strings.Fields(`
bash bat bats bind-utils bzip2 ca-certificates cargo
clang clang-tools-extra cmake coreutils curl diffutils fd-find file
findutils fzf gawk gcc gcc-c++ gdb gettext-envsubst gh git git-lfs glab
golang gopls grep gzip iproute iputils jq just less lld lldb llvm lsof
make meson mold ninja-build nmap-ncat
nodejs24 nodejs24-bin nodejs24-npm nodejs24-npm-bin
openssh-clients openssl patch pkgconf-pkg-config procps-ng
python3 python3-devel python3-pytest-xdist ripgrep rsync rust
rust-analyzer rustfmt python3-scons sed ShellCheck shfmt sqlite strace tar time
tree tree-sitter-cli unzip util-linux uv wget2-wget which xxd xz yq zip zstd
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

var debianPackageReplacements = map[string][]string{
	"ShellCheck":            {"shellcheck"},
	"bind-utils":            {"bind9-dnsutils"},
	"clang-tools-extra":     {"clang-tools"},
	"gcc-c++":               {"g++"},
	"gettext-envsubst":      {"gettext-base"},
	"helm":                  nil,
	"iproute":               {"iproute2"},
	"iputils":               {"iputils-arping", "iputils-clockdiff", "iputils-ping", "iputils-tracepath"},
	"kubernetes1.36-client": {"kubectl"},
	"man-pages":             {"manpages"},
	"nmap-ncat":             {"ncat"},
	"nodejs24":              {"nodejs"},
	"nodejs24-bin":          {"nodejs"},
	"nodejs24-npm":          {"npm"},
	"nodejs24-npm-bin":      {"npm"},
	"openssh-clients":       {"openssh-client"},
	"opentofu":              nil,
	"perf":                  {"linux-perf"},
	"pkgconf-pkg-config":    {"pkgconf"},
	"pnpm":                  nil,
	"podman":                nil,
	"podman-docker":         nil,
	"procps-ng":             {"procps"},
	"python3-devel":         {"python3-dev"},
	"python3-scons":         {"scons"},
	"redhat-rpm-config":     nil,
	"rpm-build":             {"rpm"},
	"rpmdevtools":           nil,
	"rust":                  {"rustc"},
	"sqlite":                {"sqlite3"},
	"uv":                    nil,
	"vim-enhanced":          {"vim"},
	"wget2-wget":            {"wget"},
	"which":                 {"gnu-which"},
	"xz":                    {"xz-utils"},
}

func packagesFor(distribution Distribution) ([]string, error) {
	if !distribution.Valid() {
		return nil, fmt.Errorf("unsupported distro %q", distribution)
	}
	if distribution == DistributionFedora {
		return append([]string(nil), fedoraBaselinePackages...), nil
	}
	packages := make([]string, 0, len(fedoraBaselinePackages))
	for _, pkg := range fedoraBaselinePackages {
		if replacement, ok := debianPackageReplacements[pkg]; ok {
			packages = append(packages, replacement...)
			continue
		}
		packages = append(packages, pkg)
	}
	return packages, nil
}

// BaselinePackages returns a sorted copy of the packages guaranteed in every
// VM. Callers cannot mutate the canonical manifest.
func BaselinePackages(distributions ...Distribution) []string {
	distribution := DistributionFedora
	if len(distributions) > 0 {
		distribution = distributions[0]
	}
	packages, err := packagesFor(distribution)
	if err != nil {
		return nil
	}
	return mergePackageSets(packages)
}

// PackageManifest validates operator additions and merges them into the
// guaranteed baseline. Duplicate additions, including baseline packages, are
// harmless and the resulting manifest is always sorted and duplicate-free.
func PackageManifest(additions []string, distributions ...Distribution) ([]string, error) {
	if err := ValidatePackages(additions); err != nil {
		return nil, err
	}
	distribution := DistributionFedora
	if len(distributions) > 0 {
		distribution = distributions[0]
	}
	packages, err := packagesFor(distribution)
	if err != nil {
		return nil, err
	}
	return mergePackageSets(packages, additions), nil
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

func packageArguments(packages []string) string {
	quoted := make([]string, 0, len(packages))
	for _, pkg := range packages {
		quoted = append(quoted, shellQuote(pkg))
	}
	return strings.Join(quoted, " ")
}

// DistributionSetupScript installs the distro-specific baseline and upstream
// repositories. Everything after this stage is shared between guest distros.
func DistributionSetupScript(distribution Distribution, additions []string) (string, error) {
	packages, err := PackageManifest(additions, distribution)
	if err != nil {
		return "", err
	}
	switch distribution {
	case DistributionFedora:
		return fmt.Sprintf(`#!/bin/bash
set -euo pipefail

readonly ready_marker=/var/lib/agent-os/distribution-ready
if [ -f "$ready_marker" ]; then
  exit 0
fi

dnf install -y %s
dnf install -y dnf-plugins-core
if [ ! -f /etc/yum.repos.d/hashicorp.repo ]; then
  dnf config-manager addrepo --from-repofile=https://rpm.releases.hashicorp.com/fedora/hashicorp.repo
fi
dnf install -y terraform
dnf install -y adoptium-temurin-java-repository
dnf config-manager setopt adoptium-temurin-java-repository.enabled=1
dnf install -y temurin-25-jdk

for executable in terraform java javac; do
  resolved=$(command -v "$executable")
  test -x "$resolved"
done
install -d -m 0755 /var/lib/agent-os
touch "$ready_marker"
chmod 0644 "$ready_marker"
`, packageArguments(packages)), nil
	case DistributionDebian:
		aptPackages := mergePackageSets(packages, []string{"containerd.io", "docker-buildx-plugin", "docker-ce", "docker-ce-cli", "docker-compose-plugin", "helm", "terraform", "temurin-25-jdk", "tofu"})
		return fmt.Sprintf(`#!/bin/bash
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

readonly ready_marker=/var/lib/agent-os/distribution-ready
if [ -f "$ready_marker" ]; then
  exit 0
fi

apt-get update
apt-get install -y ca-certificates curl gnupg wget
install -d -m 0755 /etc/apt/keyrings
installer_dir=$(mktemp -d /tmp/agent-os-debian-setup.XXXXXX)
cleanup() {
  rm -rf -- "$installer_dir"
}
trap cleanup EXIT

for conflicting_package in docker.io docker-compose docker-doc docker-buildx podman podman-docker containerd runc; do
  apt-get remove -y "$conflicting_package" || true
done
curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc
chmod 0644 /etc/apt/keyrings/docker.asc
docker_arch=$(dpkg --print-architecture)
cat > /etc/apt/sources.list.d/docker.sources <<AGENT_OS_DOCKER_REPOSITORY
Types: deb
URIs: https://download.docker.com/linux/debian
Suites: trixie
Components: stable
Architectures: $docker_arch
Signed-By: /etc/apt/keyrings/docker.asc
AGENT_OS_DOCKER_REPOSITORY

curl -fsSL https://packages.buildkite.com/helm-linux/helm-debian/gpgkey -o "$installer_dir/helm.gpg"
helm_fingerprint=$(gpg --show-keys --with-colons "$installer_dir/helm.gpg" | awk -F: '$1 == "fpr" {print $10; exit}')
test "$helm_fingerprint" = DDF78C3E6EBB2D2CC223C95C62BA89D07698DBC6
gpg --batch --yes --dearmor -o /etc/apt/keyrings/helm.gpg "$installer_dir/helm.gpg"
echo 'deb [signed-by=/etc/apt/keyrings/helm.gpg] https://packages.buildkite.com/helm-linux/helm-debian/any/ any main' > /etc/apt/sources.list.d/helm.list

curl -fsSL https://get.opentofu.org/opentofu.gpg -o /etc/apt/keyrings/opentofu.gpg
curl -fsSL https://packages.opentofu.org/opentofu/tofu/gpgkey -o "$installer_dir/opentofu-repo.gpg"
gpg --batch --yes --dearmor -o /etc/apt/keyrings/opentofu-repo.gpg "$installer_dir/opentofu-repo.gpg"
echo 'deb [signed-by=/etc/apt/keyrings/opentofu.gpg,/etc/apt/keyrings/opentofu-repo.gpg] https://packages.opentofu.org/opentofu/tofu/any/ any main' > /etc/apt/sources.list.d/opentofu.list

curl -fsSL https://apt.releases.hashicorp.com/gpg -o "$installer_dir/hashicorp.gpg"
gpg --batch --yes --dearmor -o /etc/apt/keyrings/hashicorp.gpg "$installer_dir/hashicorp.gpg"
echo 'deb [signed-by=/etc/apt/keyrings/hashicorp.gpg] https://apt.releases.hashicorp.com trixie main' > /etc/apt/sources.list.d/hashicorp.list

curl -fsSL https://packages.adoptium.net/artifactory/api/gpg/key/public -o "$installer_dir/adoptium.gpg"
gpg --batch --yes --dearmor -o /etc/apt/keyrings/adoptium.gpg "$installer_dir/adoptium.gpg"
echo 'deb [signed-by=/etc/apt/keyrings/adoptium.gpg] https://packages.adoptium.net/artifactory/deb trixie main' > /etc/apt/sources.list.d/adoptium.list

chmod 0644 /etc/apt/keyrings/*.gpg /etc/apt/sources.list.d/*.list
apt-get update
apt-get install -y %s

groupadd --force docker
usermod -aG docker agent
systemctl enable --now docker.service containerd.service

if [ -x /usr/bin/fdfind ] && [ ! -e /usr/local/bin/fd ]; then
  ln -s /usr/bin/fdfind /usr/local/bin/fd
fi

curl -fsSL https://get.pnpm.io/install.sh -o "$installer_dir/install-pnpm.sh"
PNPM_HOME=/usr/local SHELL=/bin/bash sh "$installer_dir/install-pnpm.sh"
curl -LsSf https://astral.sh/uv/install.sh -o "$installer_dir/install-uv.sh"
UV_UNMANAGED_INSTALL=/usr/local/bin sh "$installer_dir/install-uv.sh"

for executable in helm tofu terraform pnpm uv uvx java javac; do
  resolved=$(command -v "$executable")
  test -x "$resolved"
done
install -d -m 0755 /var/lib/agent-os
touch "$ready_marker"
chmod 0644 "$ready_marker"
`, packageArguments(aptPackages)), nil
	default:
		return "", fmt.Errorf("unsupported distro %q", distribution)
	}
}

// ChromeInstallScript installs Google Chrome Stable from Google's native
// package for the selected guest architecture. The package configures Google's
// repository so subsequent distro upgrades also upgrade Chrome.
func ChromeInstallScript(distribution Distribution, architecture string) (string, error) {
	var packageURL, extension, installCommand string
	switch distribution {
	case DistributionFedora:
		extension = "rpm"
		installCommand = `dnf install -y "$package_path"`
		switch architecture {
		case "x86_64":
			packageURL = "https://dl.google.com/linux/direct/google-chrome-stable_current_x86_64.rpm"
		case "aarch64":
			packageURL = "https://dl.google.com/dl/linux/direct/google-chrome-stable_current_aarch64.rpm"
		default:
			return "", fmt.Errorf("Google Chrome Fedora package is not published for architecture %q", architecture)
		}
	case DistributionDebian:
		extension = "deb"
		installCommand = `DEBIAN_FRONTEND=noninteractive apt-get install -y "$package_path"`
		switch architecture {
		case "x86_64":
			packageURL = "https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb"
		case "aarch64":
			packageURL = "https://dl.google.com/linux/direct/google-chrome-stable_current_arm64.deb"
		default:
			return "", fmt.Errorf("Google Chrome Debian package is not published for architecture %q", architecture)
		}
	default:
		return "", fmt.Errorf("unsupported distro %q", distribution)
	}
	return fmt.Sprintf(`#!/bin/bash
set -euo pipefail

package_path=$(mktemp /tmp/agent-os-google-chrome.XXXXXX.%s)
cleanup() {
  rm -f -- "$package_path"
}
trap cleanup EXIT
curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
  --retry 5 --output "$package_path" -- %s
%s
resolved=$(command -v google-chrome-stable)
test -x "$resolved"
`, extension, shellQuote(packageURL), installCommand), nil
}

// CodingAgentsScript returns the root-run, idempotent installer for the seven
// coding agents included in every VM. Upstream installers intentionally select
// their latest release at first boot; the marker prevents a later provisioning
// replay from unexpectedly upgrading an existing VM.
func CodingAgentsScript(configuredSkills ...[]string) string {
	skills := DefaultSkills()
	if len(configuredSkills) > 0 {
		manifest, err := SkillManifest(configuredSkills[0])
		if err != nil {
			return invalidProvisionScript(err)
		}
		skills = manifest
	}
	return `#!/bin/bash
set -euo pipefail

readonly agent_home=/home/agent
readonly ready_marker=/var/lib/agent-os/coding-agents-ready
readonly managed_path=/home/agent/.local/bin:/home/agent/.opencode/bin:/usr/local/bin:/usr/bin:/bin
readonly codex_home=/home/agent/.codex
readonly copilot_home=/home/agent/.copilot

cd "$agent_home"

if [ -f "$ready_marker" ]; then
  exit 0
fi

for executable in node npm terraform; do
  resolved=$(command -v "$executable")
  test -x "$resolved"
done
install -d -o agent -g agent -m 0755 "$agent_home/.local" "$agent_home/.local/bin" "$agent_home/.opencode/bin" "$agent_home/.agents/skills"

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
	  --connect-timeout 15 --max-time 300 \
	  --retry 10 --retry-delay 5 --retry-max-time 300 --retry-all-errors \
	  --output "$destination" "$url"
  chmod 0644 "$destination"
}

run_as_agent() {
  /usr/sbin/runuser --user agent -- /usr/bin/env \
    HOME="$agent_home" SHELL=/bin/bash PATH="$managed_path" \
    CODEX_HOME="$codex_home" COPILOT_HOME="$copilot_home" "$@"
}

download_installer https://cursor.com/install "$installer_dir/cursor.sh"
run_as_agent /bin/bash "$installer_dir/cursor.sh"

download_installer https://opencode.ai/install "$installer_dir/opencode.sh"
run_as_agent /bin/bash "$installer_dir/opencode.sh" --no-modify-path

download_installer https://chatgpt.com/codex/install.sh "$installer_dir/codex.sh"
run_as_agent /usr/bin/env CODEX_INSTALL_DIR="$agent_home/.local/bin" \
  /bin/sh "$installer_dir/codex.sh"

download_installer https://claude.ai/install.sh "$installer_dir/claude.sh"
run_as_agent /bin/bash "$installer_dir/claude.sh" latest

download_installer https://antigravity.google/cli/install.sh "$installer_dir/antigravity.sh"
run_as_agent /bin/bash "$installer_dir/antigravity.sh" --dir "$agent_home/.local/bin"

run_as_agent /usr/bin/npm config set prefix "$agent_home/.local"
run_as_agent /usr/bin/npm install --global \
  --prefix "$agent_home/.local" @devcontainers/cli@latest
run_as_agent /usr/bin/npm install --global --ignore-scripts \
  --prefix "$agent_home/.local" @earendil-works/pi-coding-agent@latest

` + chromeDevToolsInstallBlock(skills) + `

download_installer https://gh.io/copilot-install "$installer_dir/copilot.sh"
run_as_agent /usr/bin/env PREFIX="$agent_home/.local" \
  /bin/bash "$installer_dir/copilot.sh"

for executable in java javac; do
  resolved=$(command -v "$executable")
  test -x "$resolved"
done

for executable in agent opencode codex claude agy pi copilot devcontainer; do
  run_as_agent /bin/sh -c 'resolved=$(command -v "$1") && test -x "$resolved"' sh "$executable"
done

readonly path_line='export PATH="$HOME/.local/bin:$HOME/.opencode/bin:$PATH"'
for profile in "$agent_home/.bash_profile" "$agent_home/.bashrc"; do
  touch "$profile"
  chown agent:agent "$profile"
  if ! grep -Fqx "$path_line" "$profile"; then
    printf '%s\n' "$path_line" >> "$profile"
  fi
  if ! grep -Fqx 'export CODEX_HOME="$HOME/.codex"' "$profile"; then
    printf '%s\n' 'export CODEX_HOME="$HOME/.codex"' >> "$profile"
  fi
  if ! grep -Fqx 'export COPILOT_HOME="$HOME/.copilot"' "$profile"; then
    printf '%s\n' 'export COPILOT_HOME="$HOME/.copilot"' >> "$profile"
  fi
done

install -d -o root -g root -m 0755 /var/lib/agent-os
touch "$ready_marker"
chmod 0644 "$ready_marker"
`
}

// KindPodmanScript returns the root-run, idempotent setup for running kind with
// rootless Podman as the unprivileged agent user.
func KindPodmanScript() string {
	return `#!/bin/bash
set -euo pipefail

readonly agent_user=agent
readonly agent_home=/home/agent
agent_uid="$(id -u "$agent_user")"
readonly agent_uid

install -d -o "$agent_user" -g "$agent_user" -m 0755 \
  "$agent_home/.config" \
  "$agent_home/.config/containers" \
  "$agent_home/.config/containers/containers.conf.d"
cat > "$agent_home/.config/containers/containers.conf.d/agent-os-kind.conf" <<'AGENT_OS_KIND_CONTAINERS'
[containers]
log_driver = "k8s-file"
pids_limit = 65536
AGENT_OS_KIND_CONTAINERS
chown "$agent_user:$agent_user" \
  "$agent_home/.config/containers/containers.conf.d/agent-os-kind.conf"
chmod 0644 "$agent_home/.config/containers/containers.conf.d/agent-os-kind.conf"

cat > /etc/modules-load.d/agent-os-kind.conf <<'AGENT_OS_KIND_MODULES'
ip6_tables
ip6table_nat
ip_tables
iptable_nat
AGENT_OS_KIND_MODULES
chmod 0644 /etc/modules-load.d/agent-os-kind.conf
while read -r module; do
  modprobe "$module"
done < /etc/modules-load.d/agent-os-kind.conf

cat > /etc/sysctl.d/90-agent-os-kind.conf <<'AGENT_OS_KIND_SYSCTLS'
fs.inotify.max_user_watches = 524288
fs.inotify.max_user_instances = 512
AGENT_OS_KIND_SYSCTLS
chmod 0644 /etc/sysctl.d/90-agent-os-kind.conf
sysctl --system

install -d -o root -g root -m 0755 /etc/systemd/system/user@.service.d
cat > /etc/systemd/system/user@.service.d/agent-os-kind.conf <<'AGENT_OS_KIND_DELEGATION'
[Service]
Delegate=yes
AGENT_OS_KIND_DELEGATION
chmod 0644 /etc/systemd/system/user@.service.d/agent-os-kind.conf
loginctl enable-linger "$agent_user"
systemctl daemon-reload
systemctl start "user@$agent_uid.service"
systemctl is-active --quiet "user@$agent_uid.service"

cat > /usr/local/bin/kind <<'AGENT_OS_KIND_LAUNCHER'
#!/bin/bash
set -euo pipefail

user_uid="$(id -u)"
readonly user_uid
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$user_uid}"
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=$XDG_RUNTIME_DIR/bus}"
export KIND_EXPERIMENTAL_PROVIDER="${KIND_EXPERIMENTAL_PROVIDER:-podman}"

exec /usr/bin/systemd-run --user --scope --quiet \
  --property=Delegate=yes -- /usr/bin/kind "$@"
AGENT_OS_KIND_LAUNCHER
chown root:root /usr/local/bin/kind
chmod 0755 /usr/local/bin/kind

test "$(stat -fc %T /sys/fs/cgroup)" = cgroup2fs
for executable in /usr/bin/kind /usr/bin/podman /usr/bin/systemd-run /usr/local/bin/kind; do
  test -x "$executable"
done
`
}

// KindDockerScript validates Docker CE and kind for the unprivileged agent.
func KindDockerScript() string {
	return `#!/bin/bash
set -euo pipefail

groupadd --force docker
usermod -aG docker agent
systemctl enable --now docker.service containerd.service
test -x /usr/bin/docker
test -x /usr/bin/kind
systemctl is-active --quiet docker.service
/usr/sbin/runuser --user agent -- /usr/bin/env \
  HOME=/home/agent PATH=/home/agent/.local/bin:/home/agent/.opencode/bin:/usr/local/bin:/usr/bin:/bin \
  /usr/bin/docker info >/dev/null
`
}

// ContainerRuntimeScript returns the distro-specific container runtime setup.
func ContainerRuntimeScript(distribution Distribution) (string, error) {
	switch distribution {
	case DistributionFedora:
		return KindPodmanScript(), nil
	case DistributionDebian:
		return KindDockerScript(), nil
	default:
		return "", fmt.Errorf("unsupported distro %q", distribution)
	}
}

// PackageUpgradeScript upgrades every package from configured repositories.
func PackageUpgradeScript(distribution Distribution) (string, error) {
	switch distribution {
	case DistributionFedora:
		return "#!/bin/bash\nset -euo pipefail\ndnf upgrade --refresh -y\n", nil
	case DistributionDebian:
		return "#!/bin/bash\nset -euo pipefail\nexport DEBIAN_FRONTEND=noninteractive\napt-get update\napt-get full-upgrade -y\n", nil
	default:
		return "", fmt.Errorf("unsupported distro %q", distribution)
	}
}

// GlobalNPMUpgradeScript upgrades all top-level packages in the agent-owned
// global npm prefix, including packages installed outside agent-os.
func GlobalNPMUpgradeScript() string {
	return `#!/bin/bash
set -euo pipefail

readonly agent_home=/home/agent
readonly managed_path=/home/agent/.local/bin:/home/agent/.opencode/bin:/usr/local/bin:/usr/bin:/bin
manifest=$(mktemp /tmp/agent-os-global-npm.XXXXXX.json)
cleanup() {
  rm -f -- "$manifest"
}
trap cleanup EXIT

/usr/sbin/runuser --user agent -- /usr/bin/env \
  HOME="$agent_home" PATH="$managed_path" \
  /usr/bin/npm ls --global --prefix "$agent_home/.local" --depth=0 --json > "$manifest" || true
jq -r '(.dependencies // {}) | keys[]' "$manifest" | while IFS= read -r package; do
  /usr/sbin/runuser --user agent -- /usr/bin/env \
    HOME="$agent_home" PATH="$managed_path" \
    /usr/bin/npm install --global --prefix "$agent_home/.local" "${package}@latest"
done
`
}

// ProfileSetupScript attaches and validates the profile filesystem, then
// routes each documented agent state tree onto it. Existing content at one of
// those roots is copied into the profile disk before the old root is moved
// aside; no personal files are silently discarded.
func ProfileSetupScript(spec ProfileMountSpec) string {
	if spec.DiskID == "" || spec.Label == "" || strings.ContainsAny(spec.DiskID+spec.Label, "'\n\r") {
		return "#!/bin/bash\nset -eu\necho 'invalid agent-os profile identity' >&2\nexit 1\n"
	}

	var b strings.Builder
	b.WriteString("#!/bin/bash\nset -euo pipefail\n\n")
	b.WriteString("readonly profile_mount=/var/lib/agent-os/profile\n")
	b.WriteString("readonly profile_root=/var/lib/agent-os/profile\n")
	fmt.Fprintf(&b, "readonly expected_label=%s\n", shellQuote(spec.Label))
	fmt.Fprintf(&b, "readonly profile_source=%s\n", shellQuote("/mnt/lima-"+spec.DiskID))
	b.WriteString(`readonly profile_device=/dev/vdb
if ! mountpoint -q "$profile_source"; then
  test -b "$profile_device"
  filesystem=$(blkid -o value -s TYPE "$profile_device" 2>/dev/null || true)
  if [ -z "$filesystem" ]; then
    test -z "$(wipefs -n "$profile_device" 2>/dev/null || true)"
    mkfs.ext4 -F -L "$expected_label" "$profile_device"
    filesystem=ext4
  fi
  test "$filesystem" = ext4
  test "$(blkid -o value -s LABEL "$profile_device")" = "$expected_label"
  install -d -o root -g root -m 0755 "$profile_source"
  mount -t ext4 -o nodev,nosuid "$profile_device" "$profile_source"
fi
`)
	b.WriteString(`test -d "$profile_source"
mountpoint -q "$profile_source"
test "$(findmnt -no FSTYPE --target "$profile_source")" = ext4
test "$(blkid -o value -s LABEL "$profile_device")" = "$expected_label"
mount -o remount,nodev,nosuid "$profile_source"
resize2fs "$(findmnt -no SOURCE --target "$profile_source")"
if ! mountpoint -q "$profile_mount"; then
  install -d -o root -g root -m 0755 "$profile_mount"
  mount --bind "$profile_source" "$profile_mount"
fi
mount -o remount,bind,nodev,nosuid "$profile_mount"
`)
	b.WriteString(`
install -d -o agent -g agent -m 0700 "$profile_root"
install -d -o agent -g agent -m 0700 \
  "$profile_root/opencode/config" "$profile_root/opencode/data" \
  "$profile_root/orca" \
  "$profile_root/codex" "$profile_root/claude" "$profile_root/pi-agent" \
  "$profile_root/copilot" "$profile_root/agents" "$profile_root/agent-os" \
  "$profile_root/legacy"

route_profile_tree() {
  local destination=$1
  local target=$2
  install -d -o agent -g agent -m 0755 "$(dirname "$destination")"
  if [ -L "$destination" ] && [ "$(readlink -- "$destination")" = "$target" ]; then
    return
  fi
  if [ -e "$destination" ] || [ -L "$destination" ]; then
    if [ -d "$destination" ] && [ ! -L "$destination" ]; then
      cp -a -n "$destination"/. "$target"/
      moved="$profile_root/legacy/$(basename "$destination")-$(date +%s%N)"
      mv -- "$destination" "$moved"
    else
      moved="$profile_root/legacy/$(basename "$destination")-$(date +%s%N)"
      mv -- "$destination" "$moved"
    fi
  fi
  ln -s -- "$target" "$destination"
}

route_profile_tree /home/agent/.config/opencode "$profile_root/opencode/config"
route_profile_tree /home/agent/.config/orca "$profile_root/orca"
route_profile_tree /home/agent/.local/share/opencode "$profile_root/opencode/data"
route_profile_tree /home/agent/.codex "$profile_root/codex"
route_profile_tree /home/agent/.claude "$profile_root/claude"
route_profile_tree /home/agent/.pi/agent "$profile_root/pi-agent"
route_profile_tree /home/agent/.copilot "$profile_root/copilot"
route_profile_tree /home/agent/.agents "$profile_root/agents"
route_profile_tree /home/agent/.agent-os "$profile_root/agent-os"

codex_config=/home/agent/.codex/config.toml
if [ -L "$codex_config" ]; then
  echo 'refusing to follow a symlinked Codex configuration' >&2
  exit 1
fi
touch "$codex_config"
if grep -Eq '^[[:space:]]*cli_auth_credentials_store[[:space:]]*=' "$codex_config"; then
  sed -i -E 's/^[[:space:]]*cli_auth_credentials_store[[:space:]]*=.*/cli_auth_credentials_store = "file"/' "$codex_config"
else
  printf '%s\n' 'cli_auth_credentials_store = "file"' >> "$codex_config"
fi
chown agent:agent "$codex_config"
chmod 0600 "$codex_config"

if [ -f "$profile_root/claude.json" ]; then
  install -o agent -g agent -m 0600 "$profile_root/claude.json" /home/agent/.claude.json.agent-os.tmp
  mv -f -- /home/agent/.claude.json.agent-os.tmp /home/agent/.claude.json
fi
`)
	return b.String()
}

// ProfileSyncScript atomically copies Claude Code's separate configuration
// file. It is deliberately a tiny root-run command so stop/destroy/upgrade
// can fail closed if the guest cannot flush this state.
func ProfileSyncScript() string {
	return `#!/bin/bash
set -euo pipefail
readonly profile_root=/var/lib/agent-os/profile
readonly source=/home/agent/.claude.json
readonly destination=$profile_root/claude.json
case "${1:-}" in
sync)
  if [ -e "$source" ] || [ -L "$source" ]; then
    test -f "$source"
    test ! -L "$destination"
    install -o agent -g agent -m 0600 "$source" "$destination.tmp"
    mv -f -- "$destination.tmp" "$destination"
    sync
  fi
  ;;
restore)
  if [ -f "$destination" ]; then
    test ! -L "$source"
    install -o agent -g agent -m 0600 "$destination" "$source.tmp"
    mv -f -- "$source.tmp" "$source"
    sync
  fi
  ;;
*)
  echo 'usage: agent-os-profile-sync sync|restore' >&2
  exit 2
  ;;
esac
`
}

// ProfileRestoreSystemdUnit re-establishes the profile mount and restores
// Claude Code's separate configuration before Orca starts after every guest
// boot. The setup script is idempotent and is needed on Lima because its
// additional-disk bind mount is not itself persistent across a guest reboot.
func ProfileRestoreSystemdUnit() string {
	return `[Unit]
Description=agent-os persistent profile restore
After=local-fs.target
Before=orca.service
Wants=local-fs.target
RequiresMountsFor=/var/lib/agent-os/profile

[Service]
Type=oneshot
ExecStart=/usr/local/libexec/agent-os-profile-setup
ExecStart=/usr/local/libexec/agent-os-profile-sync restore
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
`
}

// AgentInstructionsScript returns a root-run, idempotent provisioning script
// for the repository instructions. It recreates only absent or stale
// agent-os-owned symlinks; unrelated regular files and directories remain
// untouched.
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
	fmt.Fprintf(&b, "# agent-os-owned instruction file (never execute): rm -rf -- %s\n", shellQuote(canonical))
	fmt.Fprintf(&b, "install -d -o %s -g %s -m 0755 %s\n", shellQuote(user), shellQuote(group), shellQuote(path.Dir(canonical)))
	fmt.Fprintf(&b, "if [ -d %s ] && [ ! -L %s ]; then mv -- %s %s.agent-os-previous; fi\n", shellQuote(canonical), shellQuote(canonical), shellQuote(canonical), shellQuote(canonical))
	fmt.Fprintf(&b, "printf '%%s' %s > %s\n", shellQuote(content), shellQuote(canonical))
	fmt.Fprintf(&b, "chown %s:%s %s\nchmod 0644 %s\n\n", shellQuote(user), shellQuote(group), shellQuote(canonical), shellQuote(canonical))
	for _, link := range links {
		// This comment documents the historical destructive operation without
		// executing it. Personal files at these destinations are preserved.
		fmt.Fprintf(&b, "# agent-os-owned link (never execute): rm -rf -- %s\n", shellQuote(link))
		fmt.Fprintf(&b, "install -d -o %s -g %s -m 0755 %s\n", shellQuote(user), shellQuote(group), shellQuote(path.Dir(link)))
		fmt.Fprintf(&b, "if [ -L %s ]; then\n  if [ \"$(readlink -- %s)\" != %s ]; then rm -f -- %s; fi\nelif [ -e %s ]; then\n  : # preserve unrelated personal content at this destination\nfi\n", shellQuote(link), shellQuote(link), shellQuote(canonical), shellQuote(link), shellQuote(link))
		fmt.Fprintf(&b, "if [ ! -e %s ] && [ ! -L %s ]; then ln -s -- %s %s; fi\n\n", shellQuote(link), shellQuote(link), shellQuote(canonical), shellQuote(link))
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

func InstallCommand(packages []string, distributions ...Distribution) []string {
	distribution := DistributionFedora
	if len(distributions) > 0 {
		distribution = distributions[0]
	}
	result := []string{"dnf", "install", "-y"}
	if distribution == DistributionDebian {
		result = []string{"apt-get", "install", "-y"}
	}
	result = append(result, packages...)
	return result
}
