# agent-os

`agent-os` provisions a disposable, isolated Fedora Server agent VM and keeps
its operational state separate from user configuration. Fedora, Ubuntu, and
stock x86_64 Arch Linux hosts use libvirt; Apple Silicon macOS uses Lima with
Virtualization.framework. Arch derivatives and Arch Linux ARM are unsupported.

The project is intentionally conservative about trust boundaries:

- configuration is loaded only from explicit flags, documented
  `AGENT_OS_*` variables, `$XDG_CONFIG_HOME/agent-os/config.yaml`, and built-in
  defaults; the current directory is never searched;
- private keys, passphrases, tokens, and destructive-operation confirmation are
  not configuration values;
- lifecycle state is stored at `$XDG_STATE_HOME/agent-os/v1/vms/<name>/state.json`
  with mode `0600`, atomic writes, and per-VM locks;
- each VM name has an independent retained sparse profile disk (10 GiB by
  default) and metadata at `$XDG_STATE_HOME/agent-os/v1/profiles/<name>/`;
  agent settings, sessions, plugins, permissions, and authentication state
  survive VM replacement, while host configuration never enters the disk;
- host commands use argument arrays and contexts, never a shell command string;
- Fedora Server and Orca RPM inputs are pinned to published SHA-256 digests
  before guest installation;
- `internal/instructions/AGENTS.md` is embedded in the release binary and
  provisioned as `/home/agent/.agent-os/AGENTS.md`, with agent-specific
  instruction paths linked to that canonical file;
- OpenCode, Codex CLI, Claude Code, Pi, and GitHub Copilot CLI are preinstalled
  for the unprivileged `agent` user;
- Chrome DevTools MCP, its `chrome-devtools` CLI, and configured GitHub skills
  are installed only inside agent VMs;
- a complete development and operations toolset, including Node.js 24,
  Eclipse Temurin 25 JDK, and Terraform, is installed during first boot;
- `create --dry-run` generates provider artifacts without touching libvirt or
  Lima; normal create/start/stop/destroy operations are explicit.

## Build and use

```sh
make check
go run . config validate
go run . setup-host
go run . create --dry-run agents
go run . start agents
go run . skills install agents
go run . destroy --yes --purge-profiles agents
go run . completion zsh > ~/.zfunc/_agent-os
```

`setup-host` displays prerequisites first. Applying Linux package changes needs
`setup-host --apply --yes`; on macOS it will not run a Homebrew installer
implicitly. On Arch Linux, setup installs the required QEMU/libvirt tooling and
enables `libvirtd.service`. `destroy` and `upgrade` also require `--yes` or an
interactive confirmation. `auth codex` performs login inside the VM and never
copies a host authentication database. The first `start` can take up to 30
minutes while DNF installs the baseline and the five coding agents resolve
their latest versions from their official upstream installers. Existing VMs
must be destroyed and recreated to receive the complete baseline; `upgrade`
does not retrofit or refresh first-boot tools. If
`internal/instructions/AGENTS.md` changes, rebuild the `agent-os` binary before
creating new VMs.

### Real VM E2E tests

The real-VM suite is local-only and opt-in. It builds the current binary,
creates a uniquely named Fedora VM with temporary configuration/state and an
ephemeral repository key, checks the complete guest installation, exercises a
restart, and destroys the VM with its retained profile disk:

```sh
make e2e
```

Apple Silicon macOS requires `limactl` and Virtualization.framework. Supported
Linux hosts are Fedora, Ubuntu, and x86_64 Arch Linux with libvirt, QEMU,
`cloud-localds`, and `nft`; use `agent-os setup-host` to review prerequisites
and `agent-os setup-host --apply --yes` to apply supported Linux host changes.
The test needs network access to download the pinned Fedora image, Orca, and
the latest guest coding-agent installers. The first boot can take up to 30
minutes and needs several GiB of free disk space.

To retain the stopped VM and its temporary state for inspection, run:

```sh
AGENT_OS_E2E_KEEP_VM=1 make e2e
```

The test prints inspection and cleanup commands. If cleanup fails, it retains
the temporary state directory and reports the failed lifecycle operation.
The compile-only check does not create a VM:

```sh
go test -tags=e2e ./e2e -run '^$'
```

The top-level `skills` configuration list accepts public GitHub tree URLs. The
built-in Chrome DevTools CLI skill is always included; configured entries are
additive. `agent-os skills install [name]` applies package and skill updates to
an existing VM. The persistent `/home/agent/.agents/skills` tree is shared by
the coding agents and survives VM replacement.

### Adding skills manually

Edit `~/.config/agent-os/config.yaml` (or the path shown by
`agent-os config show`) and add a public GitHub tree URL:

```yaml
skills:
  - https://github.com/example/repo/tree/main/path/to/skill
```

The built-in Chrome DevTools skill remains enabled. Apply the change to an
existing VM with:

```sh
agent-os skills install agents
```

Skills installed in `/home/agent/.agents/skills` are stored on the persistent
agent profile and survive ordinary VM recreation with the same name. They are
deleted only when the profile is explicitly purged with
`destroy --purge-profiles`; skills stored elsewhere are not preserved.

Set `profiles.disk_gib`, `--profile-disk-gib`, or
`AGENT_OS_PROFILE_DISK_GIB` to change the profile disk minimum. Existing disks
only grow. Ordinary `destroy` retains the disk; `destroy --purge-profiles`
deletes it after confirmed shutdown and detachment.

### Paths preserved across VM replacement

The retained profile disk is mounted at `/var/lib/agent-os/profile` inside the
VM. The following guest paths are routed onto it and survive ordinary VM
replacement with the same VM name:

| Guest path | Profile-disk path |
| --- | --- |
| `/home/agent/.config/opencode` | `opencode/config` |
| `/home/agent/.config/orca` | `orca` |
| `/home/agent/.local/share/opencode` | `opencode/data` |
| `/home/agent/.codex` | `codex` |
| `/home/agent/.claude` | `claude` |
| `/home/agent/.pi/agent` | `pi-agent` |
| `/home/agent/.copilot` | `copilot` |
| `/home/agent/.agents` | `agents` |
| `/home/agent/.agent-os` | `agent-os` |
| `/home/agent/.claude.json` | `claude.json` |

The first nine paths are symlinked into the profile disk during provisioning.
`/home/agent/.claude.json` is copied to and restored from the profile disk by
the profile sync command before shutdown and after startup. Existing content
found at a routed path is retained under `/var/lib/agent-os/profile/legacy/`
when the path is first converted. All of these paths are removed only when
the profile is explicitly purged with `destroy --purge-profiles`.

## Guest toolset and extra packages

Every new Fedora 44 VM receives the same baseline on libvirt/x86_64 and
Lima/aarch64. It includes standard shell and file utilities; Git, GitHub, and
GitLab tools; Python, Go, Rust, Node.js 24, pnpm, Java 25, and native build
toolchains;
LLVM and GNU compilers/debuggers; Podman/Buildah/Skopeo; RPM development tools;
networking and diagnostics; manuals and terminal tools; and Kubernetes 1.36,
rootless-Podman-ready kind, Helm, Kustomize, OpenTofu, and Terraform clients.
GitHub CLI and GitLab CLI both use Fedora's official `gh` and `glab` packages.
The kind launcher defaults to Podman and runs in a delegated user cgroup; no
cluster is created automatically. Existing VMs must be recreated to receive
the rootless configuration. The canonical package
inventory and first-boot installers are in `internal/provision/provision.go`.

The `packages` config key, `--packages`, and `AGENT_OS_PACKAGES` specify
additional packages. They are merged with the guaranteed baseline, sorted, and
deduplicated; none of these interfaces can remove a baseline package. Running
`agent-os packages install` without package arguments installs the merged
baseline plus configured extras in an existing VM. Explicit arguments are also
merged with the baseline.

The locked `agent` account remains unprivileged: it has no sudo policy, and
installing diagnostic, networking, container, or build packages does not grant
it extra Linux capabilities or relax the VM security model.

The release pipeline publishes only `linux/amd64` and `darwin/arm64` binaries,
with SHA-256 checksums. `install.sh` downloads into a mode-0700 temporary
directory, verifies the selected artifact, and installs to
`${HOME}/.local/bin/agent-os` by default.
