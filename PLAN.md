# Go/Cobra Agent VM Manager with Selective 12-Factor Configuration

## Summary

Implement `agent-os` as a Go/Cobra CLI, with Viper used selectively for external configuration. Retain a Bash 3.2-compatible bootstrapper only for installing verified release binaries.

Apply useful 12-factor principles—declared dependencies, immutable builds, external configuration, streamed logs, disposable command processes, and administrative one-off commands—without pretending that VM disks, lifecycle state, or interactive credentials are stateless.

## CLI, configuration, and distribution

- Provide these Cobra commands:
  - `setup-host`
  - `create [name]`
  - `start`, `stop`, `status`, `ssh`, `logs`
  - `packages install`
  - `auth <agent>`
  - `verify`, `upgrade`, `destroy`
  - `config init|validate|show`
  - `completion <shell>`
- Centralize configuration loading through Cobra's `PersistentPreRunE` and Viper, following this precedence:
  1. Explicit command-line flag or positional argument.
  2. `AGENT_OS_*` environment variable.
  3. Explicit `--config` file or `$XDG_CONFIG_HOME/agent-os/config.yaml`.
  4. Built-in default.
  5. Interactive prompt for required values still unset.
- Never search the current directory for configuration: an untrusted repository could otherwise inject CLI settings.
- Support typed settings for VM resources, access mode, Orca port, WireGuard interface/address, repository-key path, allowed CIDRs, release repository, state directory, and log format.
- Use environment names such as `AGENT_OS_VM_CPUS`, `AGENT_OS_ACCESS_MODE`, and `AGENT_OS_ORCA_PORT`.
- Bind only documented environment variables instead of accepting arbitrary keys through `AutomaticEnv`.
- Keep private-key contents, passphrases, access tokens, confirmation answers, `--force`, and destructive-operation consent out of Viper, config files, and environment variables.
- `config show --effective` reports resolved values and their sources, always redacting sensitive paths or values; `config validate` performs no mutations.
- Follow the Cobra/Viper configuration pattern of flags overriding environment, files, and defaults. [Cobra 12-factor tutorial](https://cobra.dev/docs/tutorials/12-factor-app/)
- Store operational state separately under `$XDG_STATE_HOME/agent-os/v1/vms/<name>/state.json`:
  - Versioned schema, mode 0600, atomic writes, and per-VM locking.
  - Never treat runtime state as user configuration.
  - Reconcile state against libvirt or Lima so stale local state is not authoritative.
- Build native `linux/amd64` and `darwin/arm64` binaries with pinned `go.mod`/`go.sum` dependencies.
- Publish immutable, versioned GitHub Releases with SHA-256 manifests and provenance attestations.
- Provide a Bash 3.2 `install.sh` that downloads into a mode-0700 temporary directory, verifies the checksum, and installs under `${HOME}/.local/bin` by default.

## Host and VM implementation

- Define common backend interfaces for lifecycle, networking, forwarding, provisioning, and inspection, with separate libvirt and Lima implementations.
- Invoke fixed host programs with explicit argument arrays and contexts; never use `sh -c`, `eval`, or interpolate user input into shell programs.
- `setup-host` detects and configures:
  - Fedora: `@virtualization`, `virt-install`, and required libvirt network/client components.
  - Ubuntu: `qemu-system-x86`, `qemu-utils`, `libvirt-daemon-system`, `libvirt-clients`, `virtinst`, `ovmf`, and KVM/network utilities.
  - macOS: Lima; if Homebrew is absent, explain the action and require immediate confirmation before running the official installer, then run `brew install lima`.
- Create Fedora Server 44 from an official checksum-verified image:
  - x86_64 with KVM on Linux.
  - aarch64 with Virtualization.framework on Apple Silicon.
- Default VM name to `agents`; adapt resources up to 8 vCPUs and 16 GiB RAM while retaining sufficient host capacity, with a 120 GiB sparse disk.
- Linux hardening:
  - Unprivileged QEMU.
  - Dynamic SELinux sVirt labels on Fedora or AppArmor confinement on Ubuntu.
  - Libvirt namespaces, seccomp, and cgroup device controls.
  - KVM and host CPU acceleration retained for performance.
- macOS hardening:
  - `vmType: vz`, `plain: true`.
  - Disable Rosetta, containerd, shared directories, and host mounts.
- Retain only required disk, network, entropy, and serial/control devices. Disable graphics, audio, USB, clipboard, SSH-agent forwarding, guest-agent channels, virtiofs/9p, and host socket sharing.
- Treat each CLI invocation as a short-lived reconciliation process:
  - Respect cancellation and termination signals.
  - Roll back incomplete resources where safe.
  - Leave no orphan proxy or installer processes.
  - Run persistent guest components through systemd rather than daemonizing the CLI.

## Provisioning, packages, networking, and credentials

- Create an unprivileged `agent` user with no sudo or package-manager authorization.
- Preinstall the exact requested package manifest, validating that every requested executable is present.
- Use Fedora repositories first and pinned, checksum-verified upstream artifacts only for unavailable packages.
- Additional system packages are operator-controlled through the one-off administrative command `agent-os packages install`; agents may use rootless language package managers in their home directory.
- Prompt for the repository private-key path and require the adjacent `.pub`:
  - Validate permissions, formats, and correspondence.
  - Recommend a dedicated, repo-scoped key.
  - Store the private key root-readable inside the guest and expose only a guest-local signing-agent socket to `agent`.
  - Prompt to unlock encrypted keys after boot without retaining the passphrase.
  - Never import the host's SSH configuration or agent.
- Install Orca in [headless remote-server mode](https://www.onorca.dev/docs/remote-servers) and manage it as a guest systemd service.
- Supply Orca's non-secret runtime settings through its service environment; use protected credential files or its authentication mechanism for secrets.
- Network through dedicated NAT, never bridging:
  - Permit public internet egress.
  - Block guest access to host, private/LAN, link-local, metadata, and IPv6 ULA destinations by default.
  - Allow required DHCP/DNS/NTP and explicit CIDR exceptions.
  - Default guest ingress to drop.
- Access modes:
  - `local`: expose Orca only on host `127.0.0.1`.
  - `wireguard`: require an existing host WireGuard interface and bind Orca only to its selected tunnel address.
  - Prompt for the Orca-over-WireGuard TCP port, default `6768`; do not expose the host's WireGuard UDP port inside the guest.
- Perform Codex subscription/device login inside the VM with `agent-os auth codex`; never copy the host authentication database.
- Prefer scoped project/API credentials for unattended use and document that any credential usable by the untrusted agent can potentially be exercised or exfiltrated.

## Logs, releases, and operational behavior

- Write CLI logs to stdout/stderr; create no application log files.
- Support human-readable output by default and newline-delimited JSON with `--log-format=json`.
- Keep normal command output on stdout and diagnostics on stderr; redact keys, tokens, passphrases, authentication responses, and sensitive command arguments.
- `agent-os logs` streams systemd/Orca output instead of copying it into a separate logging store.
- Separate build, release, and run:
  - CI builds and tests immutable artifacts.
  - A release is an artifact plus version metadata and checksums.
  - Runtime configuration comes from flags, documented environment variables, or an external config file.
- Model `packages install`, `auth`, `verify`, and `upgrade` as explicit one-off administrative processes.
- Do not force irrelevant 12-factor concepts:
  - VM disks and snapshots remain persistent by design.
  - Local lifecycle state remains necessary but contains no configuration secrets.
  - Libvirt and Lima are host providers, not interchangeable remote backing services.
  - The CLI does not attempt horizontal process scaling.

## Verification and tests

- Unit-test configuration precedence, explicit environment bindings, type validation, source reporting, redaction, and the prohibition on current-directory config discovery.
- Verify interactive prompts occur only for unset values on a TTY; noninteractive execution fails with actionable missing-setting errors.
- Golden-test generated libvirt XML, Lima YAML, cloud-init, systemd units, firewall rules, and package manifests.
- Test subprocess arguments, cancellation, rollback, state reconciliation, locking, and interrupted operations with fake backends.
- Test that configuration and state files never contain credentials and that logs redact sensitive values.
- Test the Bash installer under Bash 3.2, including corrupted checksums and interrupted downloads.
- Run Go unit/race/static tests, ShellCheck, and release reproducibility checks in CI.
- Run self-hosted KVM and Apple Silicon integration tests verifying:
  - Correct Fedora architecture and acceleration.
  - Every requested development tool.
  - No agent sudo or system package-management access.
  - Public internet access while host, LAN, and metadata access fail.
  - Orca reachability only through the selected binding.
  - Repository authentication without private-key file access.
  - Idempotent start, stop, upgrade, failed-create recovery, and destroy.

## Assumptions

- Supported hosts are Fedora or Ubuntu on amd64 and Apple Silicon macOS; Intel macOS is rejected.
- WireGuard is already installed and configured on the host.
- GitHub Releases is the binary distribution channel.
- Persistent confirmation of destructive or privileged operations is intentionally excluded from 12-factor configuration.
- Security-sensitive defaults cannot be weakened through a config file alone; weakening isolation requires an explicit per-invocation flag and warning.
