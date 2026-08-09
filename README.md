# agent-os

`agent-os` provisions a disposable, isolated Fedora Server agent VM and keeps
its operational state separate from user configuration. Linux uses libvirt;
Apple Silicon macOS uses Lima with Virtualization.framework.

The project is intentionally conservative about trust boundaries:

- configuration is loaded only from explicit flags, documented
  `AGENT_OS_*` variables, `$XDG_CONFIG_HOME/agent-os/config.yaml`, and built-in
  defaults; the current directory is never searched;
- private keys, passphrases, tokens, and destructive-operation confirmation are
  not configuration values;
- lifecycle state is stored at `$XDG_STATE_HOME/agent-os/v1/vms/<name>/state.json`
  with mode `0600`, atomic writes, and per-VM locks;
- host commands use argument arrays and contexts, never a shell command string;
- Fedora Server and Orca RPM inputs are pinned to published SHA-256 digests
  before guest installation;
- `internal/instructions/AGENTS.md` is embedded in the release binary and
  provisioned as `/home/agent/.agent-os/AGENTS.md`, with agent-specific
  instruction paths linked to that canonical file;
- OpenCode, Codex CLI, Claude Code, Pi, and GitHub Copilot CLI are preinstalled
  for the unprivileged `agent` user;
- `create --dry-run` generates provider artifacts without touching libvirt or
  Lima; normal create/start/stop/destroy operations are explicit.

## Build and use

```sh
make check
go run . config validate
go run . setup-host
go run . create --dry-run agents
go run . start agents
go run . completion zsh > ~/.zfunc/_agent-os
```

`setup-host` displays prerequisites first. Applying Linux package changes needs
`setup-host --apply --yes`; on macOS it will not run a Homebrew installer
implicitly. `destroy` and `upgrade` also require `--yes` or an interactive
confirmation. `auth codex` performs login inside the VM and never copies a host
authentication database. The first `start` can take several minutes while the
five coding agents resolve their latest versions from their official upstream
installers. Existing VMs must be recreated to receive them; `upgrade` does not
retrofit or refresh the agents. If
`internal/instructions/AGENTS.md` changes, rebuild the `agent-os` binary before
creating new VMs.

The release pipeline publishes only `linux/amd64` and `darwin/arm64` binaries,
with SHA-256 checksums. `install.sh` downloads into a mode-0700 temporary
directory, verifies the selected artifact, and installs to
`${HOME}/.local/bin/agent-os` by default.
