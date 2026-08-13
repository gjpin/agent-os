# Remote quick start

## Table of contents

- [Remote setup](#remote-quick-start)
  - [Configure and start the VM](#configure-and-start-the-vm)
  - [Connect with VS Code Remote SSH](#connect-with-vs-code-remote-ssh)
  - [Connect Remote Orca](#connect-remote-orca)
- [Authenticate coding agents](#authenticate-coding-agents)
- [Local Apple Silicon macOS setup](#run-and-access-the-vm-locally-on-apple-silicon-macos)
  - [VS Code on the local VM](#vs-code-on-the-local-vm)
  - [Local Remote Orca](#local-remote-orca)

This guide assumes `agent-os` is installed on a remote Linux server and that
the server and laptop are connected by WireGuard.

Example values used below:

- Server WireGuard address: `10.44.0.1/24`
- Laptop WireGuard address: `10.44.0.2/24`
- WireGuard interface on the server: `wg0`
- Server login user: `ops`
- VM name: `agents`
- Orca port: `6768`

Replace these values with the ones from your WireGuard configuration.

## Configure and start the VM

Run these commands on the remote server as the account that owns the
`agent-os` state and can use `sudo`. Do not run the application itself with
`sudo`, because that can select a different configuration and state directory.

If the configuration does not exist, initialize it:

```sh
agent-os config init
$EDITOR ~/.config/agent-os/config.yaml
```

Set the access-related values in the configuration:

```yaml
vm:
  name: agents

access:
  mode: wireguard

orca:
  port: 6768

wireguard:
  interface: wg0
  address: 10.44.0.1/24
```

`wireguard.address` is the server's WireGuard address, not the laptop's.

Review host prerequisites and validate the configuration:

```sh
agent-os setup-host
agent-os config validate
```

If prerequisites are missing, apply the supported host setup:

```sh
agent-os setup-host --apply --yes
```

Create the VM once, then start it:

```sh
agent-os create agents
agent-os start agents
agent-os status agents
```

The first start can take up to 30 minutes while Fedora, the coding agents,
including Antigravity CLI, Orca, and the shared skills are installed.

For automatic startup after a server reboot:

```sh
agent-os autostart enable agents
```

If the VM was originally created with `access.mode: local`, recreate it after
changing the access mode so that the guest Orca service receives the new
pairing address:

```sh
agent-os stop agents
agent-os destroy --yes agents
agent-os create agents
agent-os start agents
```

The ordinary `destroy` command retains the persistent profile disk. Do not use
`--purge-profiles` unless you also want to delete the VM's settings, sessions,
skills, and authentication state.

## Connect with VS Code Remote SSH

The WireGuard tunnel lets the laptop SSH to the remote server without exposing
SSH publicly. Add an entry to `~/.ssh/config` on the laptop:

```sshconfig
Host agents-server
    HostName 10.44.0.1
    User ops
    IdentityFile ~/.ssh/id_ed25519
    IdentitiesOnly yes
    ServerAliveInterval 30
```

Test the connection:

```sh
ssh agents-server
```

In VS Code, install **Remote - SSH**, then use:

```text
Remote-SSH: Connect to Host... → agents-server
```

This connects VS Code to the remote server. From the VS Code terminal, the
VM's supported interactive shell is:

```sh
agent-os status agents
agent-os ssh agents
```

The current VM setup does not expose the guest as a normal SSH target. The
`agent-os ssh` command uses the VM's management channel, and the host-side
WireGuard forwarding exposes Orca's port, not guest SSH port 22. Therefore,
do not configure VS Code Remote SSH to connect to `10.44.0.1:6768`; port
`6768` is Orca's WebSocket endpoint.

For a full VS Code session directly inside the VM, the VM would need explicit
SSH-server provisioning and guest SSH port forwarding. Remote Orca is the
supported full-workspace interface for the current setup.

## Connect Remote Orca

`agent-os` starts Orca inside the VM automatically. It uses the WireGuard
address as Orca's advertised pairing address and port `6768`.

On the remote server, retrieve the Orca startup output:

```sh
agent-os logs agents
```

Look for a block like:

```text
Advertised endpoint: ws://10.44.0.1:6768
Pairing URL: orca://pair?code=...
```

If needed, inspect the service from inside the VM:

```sh
agent-os ssh agents -- systemctl status orca.service --no-pager
```

On the laptop, open Orca and choose:

```text
Settings → Remote Orca Servers → Add Server
```

Paste the complete `orca://pair?...` URL and connect. The pairing URL grants
access to the Orca runtime, so treat it like a password.

Test the WireGuard-to-Orca network path from the laptop with:

```sh
nc -vz 10.44.0.1 6768
```

If this fails, verify that:

- WireGuard is up on both machines and the laptop routes the server's tunnel
  address through the tunnel;
- the server's host firewall permits TCP `6768` on `wg0`;
- the WireGuard peer configuration permits the laptop's tunnel address; and
- the VM is running with `agent-os status agents`.

Do not start a second `orca serve` process on the server host. The configured
Orca service runs inside the `agents` VM.

## Authenticate coding agents

The VM must be running before you authenticate an agent. Run these commands as
the normal `agent-os` operator account:

- In the remote setup, SSH to the server first, then run the command there.
- In the local macOS setup, run the command directly on the Mac.

```sh
agent-os status agents
agent-os start agents              # only if the VM is stopped
```

Each command starts an interactive login inside the VM:

| Agent | Command | Login flow |
| --- | --- | --- |
| Codex | `agent-os auth codex` | Orca opens the Codex account flow. Follow its device or browser instructions. |
| Claude | `agent-os auth claude` | Orca opens the Claude account flow. Follow its device or browser instructions. |
| OpenCode | `agent-os auth opencode` | Complete the provider login shown by OpenCode. |
| GitHub Copilot | `agent-os auth copilot` | Complete the device login shown by Copilot. |
| Pi | `agent-os auth pi` | Pi starts in the terminal; enter `/login`, then complete the provider login. |

For example, on a remote server:

```sh
ssh agents-server
agent-os auth codex
```

On the local Mac, use the same command without SSH:

```sh
agent-os auth codex
```

Authentication state is stored inside the VM's persistent agent profile. It
survives an ordinary VM stop, start, or destroy/recreate cycle with the same
VM name. It is deleted only when the profile is explicitly purged:

```sh
agent-os destroy --yes --purge-profiles agents
```

The host's coding-agent authentication databases are not copied into the VM.

Google Antigravity CLI is available as `agy`. Start its interactive terminal
client inside the VM with:

```sh
agent-os ssh agents -- agy
```

## Run and access the VM locally on Apple Silicon macOS

On Apple Silicon macOS, `agent-os` uses Lima with Apple's
Virtualization.framework. This is a local setup: the VM and its Orca endpoint
run on your Mac.

This backend requires Apple Silicon. Intel macOS is not supported by the
current project.

Install Lima if it is not already installed, then verify the host setup:

```sh
brew install lima
agent-os setup-host
```

Create a local configuration if necessary:

```sh
agent-os config init
$EDITOR ~/.config/agent-os/config.yaml
```

Use local access mode:

```yaml
vm:
  name: agents

access:
  mode: local

orca:
  port: 6768
```

Validate and create the VM on the Mac:

```sh
agent-os config validate
agent-os create agents
agent-os start agents
agent-os status agents
```

The first start can take up to 30 minutes while the Fedora guest and its
tools are provisioned. The VM's persistent profile is stored in Lima's disk
store and is retained across an ordinary VM destroy/recreate cycle.

Open an interactive shell as the unprivileged `agent` user with:

```sh
agent-os ssh agents
```

Run a single command in the VM with:

```sh
agent-os ssh agents -- pwd
agent-os ssh agents -- systemctl status orca.service --no-pager
```

You can also use Lima directly for provider-level inspection:

```sh
limactl list agents
limactl shell agents
```

To stop and restart the VM:

```sh
agent-os stop agents
agent-os start agents
```

To start it automatically when you log in or boot the Mac, use Lima 2.2 or
newer and register autostart:

```sh
agent-os autostart enable agents
agent-os autostart status agents
```

### VS Code on the local VM

The generated Lima profile includes an SSH configuration for the VM. Add this
to `~/.ssh/config` on the Mac:

```sshconfig
Include ~/.lima/*/ssh.config
```

Find the generated config path and test the Lima SSH alias:

```sh
limactl ls --format='{{.SSHConfigFile}}' agents
ssh -F "$(limactl ls --format='{{.SSHConfigFile}}' agents)" lima-agents
```

Install VS Code's **Remote - SSH** extension and select
`lima-agents` from **Remote-SSH: Connect to Host...**. This opens the VS Code
workspace directly inside the VM.

If you use a custom `LIMA_HOME`, the SSH config is under that directory rather
than `~/.lima`; use the path printed by `limactl ls` when configuring VS Code.

### Local Remote Orca

In local mode, `agent-os` forwards the VM's Orca port to the Mac at
`127.0.0.1:6768`. Get the pairing URL with:

```sh
agent-os logs agents
```

Look for:

```text
Advertised endpoint: ws://127.0.0.1:6768
Pairing URL: orca://pair?code=...
```

In the Orca desktop app on the same Mac, choose:

```text
Settings → Remote Orca Servers → Add Server
```

Paste the complete pairing URL. Verify the local endpoint if necessary:

```sh
nc -vz 127.0.0.1 6768
```

Do not use the local-mode pairing URL from another computer. `127.0.0.1`
refers to the computer making the connection, and this setup intentionally
does not publish Orca on the Mac's LAN.
