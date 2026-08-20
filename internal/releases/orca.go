package releases

import (
	"fmt"

	"github.com/gjpin/agent-os/internal/provision"
)

const OrcaVersion = "1.4.176"

type OrcaPackage struct {
	Architecture string
	URL          string
	SHA256       string
	Extension    string
}

func OrcaRPM(architecture string) (OrcaPackage, error) {
	packages := map[string]OrcaPackage{
		"x86_64": {
			Architecture: "x86_64",
			URL:          "https://github.com/stablyai/orca/releases/download/v1.4.176/orca-ide-1.4.176.x86_64.rpm",
			SHA256:       "9bebf5ffad4d1d25ce022a0cac089c0fce23890af8024f9ecd32df51f3b951c8",
			Extension:    "rpm",
		},
		"aarch64": {
			Architecture: "aarch64",
			URL:          "https://github.com/stablyai/orca/releases/download/v1.4.176/orca-ide-1.4.176.aarch64.rpm",
			SHA256:       "3b97bf6f8a7bfa00bd66a49718d1f17ab0c8849d72f563ee202b1515e66ee0ae",
			Extension:    "rpm",
		},
	}
	value, ok := packages[architecture]
	if !ok {
		return OrcaPackage{}, fmt.Errorf("Orca %s is not published for architecture %q", OrcaVersion, architecture)
	}
	return value, nil
}

func OrcaDEB(architecture string) (OrcaPackage, error) {
	packages := map[string]OrcaPackage{
		"x86_64": {
			Architecture: "x86_64",
			URL:          "https://github.com/stablyai/orca/releases/download/v1.4.176/orca-ide_1.4.176_amd64.deb",
			SHA256:       "1c22a6a0c49a2a10c7ed26fafab7a034b569bf1d7f15c60b8760853a7f4a41d0",
			Extension:    "deb",
		},
		"aarch64": {
			Architecture: "aarch64",
			URL:          "https://github.com/stablyai/orca/releases/download/v1.4.176/orca-ide_1.4.176_arm64.deb",
			SHA256:       "10f5974c42076e50df238d1a2b89b6842aa6c6c9d5d11549f459dfb5a299245c",
			Extension:    "deb",
		},
	}
	value, ok := packages[architecture]
	if !ok {
		return OrcaPackage{}, fmt.Errorf("Orca %s Debian package is not published for architecture %q", OrcaVersion, architecture)
	}
	return value, nil
}

func OrcaInstallScript(distribution provision.Distribution, architecture string) (string, error) {
	var packageInfo OrcaPackage
	var installCommand string
	var err error
	switch distribution {
	case provision.DistributionFedora:
		packageInfo, err = OrcaRPM(architecture)
		installCommand = `/usr/bin/dnf install -y "$tmp"`
	case provision.DistributionDebian:
		packageInfo, err = OrcaDEB(architecture)
		installCommand = `DEBIAN_FRONTEND=noninteractive /usr/bin/apt-get install -y "$tmp"`
	default:
		return "", fmt.Errorf("unsupported distro %q", distribution)
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`#!/bin/bash
set -eu
umask 077
tmp="$(mktemp /run/agent-os-orca.XXXXXX.%s)"
trap 'rm -f "$tmp"' EXIT HUP INT TERM
/usr/bin/curl --fail --location --proto '=https' --tlsv1.2 --retry 3 --output "$tmp" -- '%s'
expected='%s'
actual="$(/usr/bin/sha256sum "$tmp" | /usr/bin/awk '{print $1}')"
if [ "$actual" != "$expected" ]; then
  echo 'agent-os: Orca checksum verification failed' >&2
  exit 1
fi
%s
/usr/bin/test -x /usr/bin/orca-ide
if [ ! -e /usr/bin/orca ] && [ ! -L /usr/bin/orca ]; then
  /usr/bin/ln -s /usr/bin/orca-ide /usr/bin/orca
fi
/usr/bin/test -x /usr/bin/orca
`, packageInfo.Extension, packageInfo.URL, packageInfo.SHA256, installCommand), nil
}
