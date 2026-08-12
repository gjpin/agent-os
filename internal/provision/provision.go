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

// ProfileMountSpec describes the provider-specific block-device presentation
// without putting host paths or credentials into guest configuration.
type ProfileMountSpec struct {
	Backend string
	DiskID  string
	Label   string
}

// baselinePackages is the development and operations toolset guaranteed in
// every newly provisioned VM. Provider-specific packages are added by the
// provider artifact generator, while configured packages are additive extras.
var baselinePackages = strings.Fields(`
bash bat bats bind-utils bzip2 ca-certificates cargo chromium
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

// CodingAgentsScript returns the root-run, idempotent installer for the six
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

dnf install -y dnf-plugins-core
dnf config-manager addrepo --from-repofile=https://rpm.releases.hashicorp.com/fedora/hashicorp.repo
dnf install -y terraform

dnf install -y adoptium-temurin-java-repository
dnf config-manager setopt adoptium-temurin-java-repository.enabled=1
dnf install -y temurin-25-jdk

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

run_as_agent /usr/bin/npm install --global --ignore-scripts \
  --prefix "$agent_home/.local" @earendil-works/pi-coding-agent

` + chromeDevToolsInstallBlock(skills) + `

download_installer https://gh.io/copilot-install "$installer_dir/copilot.sh"
run_as_agent /usr/bin/env PREFIX="$agent_home/.local" \
  /bin/bash "$installer_dir/copilot.sh"

for executable in java javac; do
  resolved=$(command -v "$executable")
  test -x "$resolved"
done

for executable in agent opencode codex claude pi copilot; do
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

// ProfileSetupScript attaches and validates the profile filesystem, then
// routes each documented agent state tree onto it. Existing content at one of
// those roots is copied into the profile disk before the old root is moved
// aside; no personal files are silently discarded.
func ProfileSetupScript(spec ProfileMountSpec) string {
	if spec.Backend != "lima" && spec.Backend != "libvirt" {
		return "#!/bin/bash\nset -eu\necho 'invalid agent-os profile backend' >&2\nexit 1\n"
	}
	if spec.DiskID == "" || spec.Label == "" || strings.ContainsAny(spec.DiskID+spec.Label, "'\n\r") {
		return "#!/bin/bash\nset -eu\necho 'invalid agent-os profile identity' >&2\nexit 1\n"
	}

	var b strings.Builder
	b.WriteString("#!/bin/bash\nset -euo pipefail\n\n")
	b.WriteString("readonly profile_mount=/var/lib/agent-os/profile\n")
	b.WriteString("readonly profile_root=/var/lib/agent-os/profile\n")
	fmt.Fprintf(&b, "readonly expected_label=%s\n", shellQuote(spec.Label))
	if spec.Backend == "lima" {
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
	} else {
		fmt.Fprintf(&b, "readonly profile_device=%s\n", shellQuote("/dev/disk/by-id/virtio-"+spec.DiskID))
		b.WriteString(`test -e "$profile_device"
filesystem=$(blkid -o value -s TYPE "$profile_device" 2>/dev/null || true)
if [ -z "$filesystem" ]; then
  test -z "$(wipefs -n "$profile_device" 2>/dev/null || true)"
  mkfs.ext4 -F -L "$expected_label" "$profile_device"
  filesystem=ext4
fi
test "$filesystem" = ext4
test "$(blkid -o value -s LABEL "$profile_device")" = "$expected_label"
profile_uuid=$(blkid -o value -s UUID "$profile_device")
test -n "$profile_uuid"
install -d -o root -g root -m 0755 "$profile_mount"
fstab_line="UUID=$profile_uuid $profile_mount ext4 nodev,nosuid 0 2"
if grep -Fq "UUID=$profile_uuid $profile_mount" /etc/fstab; then
  grep -Fqx "$fstab_line" /etc/fstab
else
  printf '%s\n' "$fstab_line" >> /etc/fstab
fi
if ! mountpoint -q "$profile_mount"; then
mount "$profile_mount"
fi
mount -o remount,nodev,nosuid "$profile_mount"
resize2fs "$profile_device"
`)
	}
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
	if user != "agent" || group != "agent" {
		return legacyAgentInstructionsScriptAt(content, home, user, group)
	}
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

// legacyAgentInstructionsScriptAt preserves the old helper contract for
// callers that explicitly supply numeric ownership values. Production
// provisioning always uses the safe agent/agent path above.
func legacyAgentInstructionsScriptAt(content, home, user, group string) string {
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
	result := []string{"dnf", "install", "-y"}
	result = append(result, packages...)
	return result
}

func AgentUserCloudInit() string {
	return "name: agent\n  shell: /bin/bash\n  lock_passwd: true\n  groups: []\n  sudo: []\n"
}
