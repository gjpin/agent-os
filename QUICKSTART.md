# Quickstart

`agent-os` supports x86_64 Linux through Lima/QEMU and Apple Silicon macOS
through Lima/VZ. It does not install or configure host packages.

## Debian x86_64 host

Install QEMU and the utilities used to download the upstream Lima release:

```sh
sudo apt-get update
sudo apt-get install -y qemu-system-x86 qemu-utils curl jq
```

Install Lima 2.2 or newer from the official release archive. The archive
contains `limactl` and its supporting files, so extract the whole archive rather
than copying only the executable:

```sh
LIMA_VERSION="$(curl -fsSL https://api.github.com/repos/lima-vm/lima/releases/latest | jq -r .tag_name)"
curl -fsSL "https://github.com/lima-vm/lima/releases/download/${LIMA_VERSION}/lima-${LIMA_VERSION#v}-Linux-x86_64.tar.gz" \
  | sudo tar -C /usr/local -xz
```

Verify the installation and QEMU driver:

```sh
limactl --version
qemu-system-x86_64 --version
test -r /dev/kvm && test -w /dev/kvm
```

If the KVM check fails because of group permissions, add your user to Debian's
`kvm` group and log out and back in before retrying:

```sh
sudo usermod -aG kvm "$USER"
```

Lima recommends QEMU 6.2 or newer on Linux. The generated VM uses the host's
native x86_64 architecture and explicitly selects `vmType: qemu`.

## Apple Silicon macOS host

Install Lima 2.2 or newer, then verify it:

```sh
brew install lima
limactl --version
```

The generated VM uses the native ARM64 architecture and explicitly selects
`vmType: vz` for Virtualization.framework.

## Build and create a VM

```sh
go build -o bin/agent-os .
bin/agent-os create agents
bin/agent-os start agents
bin/agent-os verify agents
```

Useful lifecycle commands:

```sh
bin/agent-os status agents
bin/agent-os ssh agents
bin/agent-os logs agents
bin/agent-os stop agents
bin/agent-os destroy --yes agents
bin/agent-os destroy --yes --purge-profiles agents
```

The normal destroy operation retains the Lima-managed profile disk. Use
`--purge-profiles` only when the retained agent profile should also be deleted.

## Autostart

Lima 2.2 or newer is required:

```sh
bin/agent-os autostart enable agents
bin/agent-os autostart status agents
bin/agent-os autostart disable agents
```

On Linux, enable registers upstream Lima login autostart as a systemd user
service. The VM starts when that user logs in. On macOS, agent-os requests
Lima's boot condition, which registers a system LaunchDaemon and may prompt for
administrator authorization. Disabling uses the same cross-platform Lima
command.

For direct inspection:

```sh
limactl list
limactl disk list
limactl shell agents
```

Existing libvirt VMs, state, and profile disks are not migrated or cleaned up.
Non-Lima state and profile metadata is rejected.

## References

- [Lima installation](https://lima-vm.io/docs/installation/)
- [Lima QEMU driver](https://lima-vm.io/docs/config/vmtype/qemu/)
- [Lima autostart](https://lima-vm.io/docs/usage/autostart/)
