package releases

import (
	"fmt"
	"strings"

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
		installCommand = `DEBIAN_FRONTEND=noninteractive /usr/bin/apt-get install -y --allow-downgrades "$tmp"`
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
%s
if [ ! -e /usr/bin/orca ] && [ ! -L /usr/bin/orca ]; then
  /usr/bin/ln -s /usr/bin/orca-ide /usr/bin/orca
fi
/usr/bin/test -x /usr/bin/orca
`, packageInfo.Extension, packageInfo.URL, packageInfo.SHA256, installCommand, orcaCLILinkScript()), nil
}

// OrcaLatestInstallScript installs the latest stable GitHub release and
// verifies the SHA-256 digest published with the selected release asset.
func OrcaLatestInstallScript(distribution provision.Distribution, architecture string) (string, error) {
	var assetPattern, extension, installCommand string
	switch distribution {
	case provision.DistributionFedora:
		extension = "rpm"
		installCommand = `/usr/bin/dnf install -y "$package_path"`
		if architecture == "x86_64" {
			assetPattern = `^orca-ide-[0-9.]+\.x86_64\.rpm$`
		} else if architecture == "aarch64" {
			assetPattern = `^orca-ide-[0-9.]+\.aarch64\.rpm$`
		}
	case provision.DistributionDebian:
		extension = "deb"
		installCommand = `DEBIAN_FRONTEND=noninteractive /usr/bin/apt-get install -y --allow-downgrades "$package_path"`
		if architecture == "x86_64" {
			assetPattern = `^orca-ide_[0-9.]+_amd64\.deb$`
		} else if architecture == "aarch64" {
			assetPattern = `^orca-ide_[0-9.]+_arm64\.deb$`
		}
	default:
		return "", fmt.Errorf("unsupported distro %q", distribution)
	}
	if assetPattern == "" {
		return "", fmt.Errorf("Orca latest package is not supported for architecture %q", architecture)
	}
	return fmt.Sprintf(`#!/bin/bash
set -euo pipefail
umask 077

metadata=$(mktemp /tmp/agent-os-orca-release.XXXXXX.json)
package_path=$(mktemp /tmp/agent-os-orca.XXXXXX.%s)
cleanup() {
  rm -f -- "$metadata" "$package_path"
}
trap cleanup EXIT
/usr/bin/curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
  --retry 5 --output "$metadata" -- https://api.github.com/repos/stablyai/orca/releases/latest
asset=$(/usr/bin/jq -cer --arg pattern %s \
  '[.assets[] | select(.name | test($pattern))] | if length == 1 then .[0] else error("expected exactly one Orca package") end' "$metadata")
url=$(printf '%%s' "$asset" | /usr/bin/jq -er '.browser_download_url | select(startswith("https://github.com/stablyai/orca/releases/download/"))')
expected=$(printf '%%s' "$asset" | /usr/bin/jq -er '.digest | select(startswith("sha256:")) | sub("^sha256:"; "")')
/usr/bin/curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
  --retry 5 --output "$package_path" -- "$url"
actual=$(/usr/bin/sha256sum "$package_path" | /usr/bin/awk '{print $1}')
test "$actual" = "$expected"
%s
%s
if [ ! -e /usr/bin/orca ] && [ ! -L /usr/bin/orca ]; then
  /usr/bin/ln -s /usr/bin/orca-ide /usr/bin/orca
fi
/usr/bin/test -x /usr/bin/orca
`, extension, shellQuoteForScript(assetPattern), installCommand, orcaCLILinkScript()), nil
}

func orcaCLILinkScript() string {
	return `for dir in /opt/Orca /opt/orca-ide /opt/orca; do
  shim="$dir/resources/bin/orca-ide"
  if [ -x "$shim" ]; then
    /usr/bin/ln -sfn "$shim" /usr/bin/orca-ide
    break
  fi
done
/usr/bin/test -x /usr/bin/orca-ide`
}

func shellQuoteForScript(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
