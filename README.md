# agent-os

## Description

`agent-os` manages isolated Fedora virtual machines for coding agents. It uses
[Lima](https://lima-vm.io/) everywhere: QEMU on x86_64 Linux and
Virtualization.framework (`vz`) on Apple Silicon macOS.

## Features

- Creates, starts, stops, upgrades, verifies, and destroys agent VMs.
- Keeps agent credentials and configuration on a Lima-managed persistent disk.
- Uses isolated plain-mode VMs with no host mounts, containerd, or Rosetta.
- Provides static local or WireGuard-bound forwarding to Orca.
- Runs guest commands through `limactl shell`.
- Supports autostart at Linux user login or macOS system boot with Lima 2.2+.

Supported hosts are `linux/amd64` and `darwin/arm64`. Lima and, on Linux, QEMU
must be installed by the operator. See [QUICKSTART.md](QUICKSTART.md) for Debian
and macOS prerequisites.

## Quickstart

```sh
go build -o bin/agent-os .
bin/agent-os create agents
bin/agent-os start agents
bin/agent-os verify agents
bin/agent-os ssh agents
```

Enable autostart with:

```sh
bin/agent-os autostart enable agents
```

On Linux this registers a systemd user service for the next login. On macOS it
registers a system LaunchDaemon for boot. Existing libvirt VMs, state, and
profile disks are not migrated or deleted automatically; non-Lima metadata is
rejected.

## Build and Test

```sh
make build
make test
make race
make vet
```

Compile the E2E suite without creating a VM:

```sh
go test -tags=e2e ./e2e -run '^$'
```

Run the real-VM suite on a supported, configured host:

```sh
make e2e
```
