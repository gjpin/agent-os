package releases

import "fmt"

const OrcaVersion = "1.4.176"

type OrcaPackage struct {
	Architecture string
	URL          string
	SHA256       string
}

func OrcaRPM(architecture string) (OrcaPackage, error) {
	packages := map[string]OrcaPackage{
		"x86_64": {
			Architecture: "x86_64",
			URL:          "https://github.com/stablyai/orca/releases/download/v1.4.176/orca-ide-1.4.176.x86_64.rpm",
			SHA256:       "9bebf5ffad4d1d25ce022a0cac089c0fce23890af8024f9ecd32df51f3b951c8",
		},
		"aarch64": {
			Architecture: "aarch64",
			URL:          "https://github.com/stablyai/orca/releases/download/v1.4.176/orca-ide-1.4.176.aarch64.rpm",
			SHA256:       "3b97bf6f8a7bfa00bd66a49718d1f17ab0c8849d72f563ee202b1515e66ee0ae",
		},
	}
	value, ok := packages[architecture]
	if !ok {
		return OrcaPackage{}, fmt.Errorf("Orca %s is not published for architecture %q", OrcaVersion, architecture)
	}
	return value, nil
}

func OrcaInstallScript(architecture string) (string, error) {
	packageInfo, err := OrcaRPM(architecture)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`#!/bin/bash
set -eu
umask 077
tmp="$(mktemp /run/agent-os-orca.XXXXXX.rpm)"
trap 'rm -f "$tmp"' EXIT HUP INT TERM
/usr/bin/curl --fail --location --proto '=https' --tlsv1.2 --retry 3 --output "$tmp" -- '%s'
expected='%s'
actual="$(/usr/bin/sha256sum "$tmp" | /usr/bin/awk '{print $1}')"
if [ "$actual" != "$expected" ]; then
  echo 'agent-os: Orca checksum verification failed' >&2
  exit 1
fi
/usr/bin/dnf install -y "$tmp"
/usr/bin/test -x /usr/bin/orca
`, packageInfo.URL, packageInfo.SHA256), nil
}
