package host

import (
	"os"
	"strings"
	"testing"

	"github.com/gjpin/agent-os/internal/backend"
)

func TestDetectBytesSelectsOnlySupportedLinuxDistributions(t *testing.T) {
	tests := []struct {
		name     string
		release  string
		distro   string
		provider string
	}{
		{
			name:     "fedora",
			release:  "NAME=\"Fedora Linux\"\nID=fedora\nVERSION_ID=40\n",
			distro:   "fedora",
			provider: "libvirt",
		},
		{
			name:     "quoted uppercase ubuntu",
			release:  "NAME='Ubuntu'\nID=\"Ubuntu\"\nID_LIKE=debian\n",
			distro:   "ubuntu",
			provider: "libvirt",
		},
		{
			name:     "arch",
			release:  "NAME=\"Arch Linux\"\nID=arch\n",
			distro:   "arch",
			provider: "libvirt",
		},
		{
			name:     "arch derivative",
			release:  "NAME=Manjaro\nID=manjaro\nID_LIKE=arch\n",
			distro:   "manjaro",
			provider: "unsupported",
		},
		{
			name:     "debian",
			release:  "NAME=Debian\nID=debian\n",
			distro:   "debian",
			provider: "unsupported",
		},
		{
			name:     "fedora derivative is not exact support",
			release:  "NAME=Rocky Linux\nID=rocky\nID_LIKE=\"rhel fedora\"\n",
			distro:   "rocky",
			provider: "unsupported",
		},
		{
			name:     "missing ID",
			release:  "NAME=Unknown Linux\nID_LIKE=ubuntu\n",
			distro:   "",
			provider: "unsupported",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			info := DetectBytes("linux", "amd64", []byte(test.release))
			if info.Distribution != test.distro || info.Provider != test.provider {
				t.Fatalf("unexpected detection: %+v", info)
			}
			if (test.provider == "libvirt") != (info.DistributionFamily != "") {
				t.Fatalf("unexpected distribution family: %+v", info)
			}
		})
	}
}

func TestOSReleaseParserHandlesCommentsWhitespaceAndQuotes(t *testing.T) {
	values := parseOSRelease(strings.Join([]string{
		"  # comment",
		" ID = 'Ubuntu'",
		"NAME=\"Ubuntu 24.04\"",
		"BROKEN",
		"EMPTY=",
	}, "\n"))
	if values["ID"] != "Ubuntu" || values["NAME"] != "Ubuntu 24.04" || values["EMPTY"] != "" {
		t.Fatalf("unexpected os-release values: %#v", values)
	}
}

func TestDetectPreservesMacOSProviderSelection(t *testing.T) {
	for _, architecture := range []string{"arm64", "amd64"} {
		info := DetectBytes("darwin", architecture, nil)
		if info.Provider != "lima" || info.Distribution != "" {
			t.Fatalf("unexpected macOS detection for %s: %+v", architecture, info)
		}
	}

	if _, err := Provider(Info{OS: "darwin", Architecture: "amd64", Provider: "lima"}, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "Intel macOS") {
		t.Fatalf("unexpected Intel macOS result: %v", err)
	}
}

func TestProviderRejectsUnsupportedLinuxDistribution(t *testing.T) {
	for _, distro := range []string{"debian", ""} {
		_, err := Provider(Info{
			OS:           "linux",
			Architecture: "amd64",
			Provider:     "libvirt",
			Distribution: distro,
		}, nil, nil, nil)
		if err == nil {
			t.Fatalf("Provider accepted unsupported distribution %q", distro)
		}
	}

	p, err := Provider(Info{
		OS:           "linux",
		Architecture: "amd64",
		Provider:     "libvirt",
		Distribution: "fedora",
	}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(backend.Libvirt); !ok {
		t.Fatalf("unexpected provider type %T", p)
	}

	p, err = Provider(Info{
		OS:           "linux",
		Architecture: "amd64",
		Provider:     "libvirt",
		Distribution: "arch",
	}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(backend.Libvirt); !ok {
		t.Fatalf("unexpected Arch provider type %T", p)
	}

	_, err = Provider(Info{OS: "linux", Architecture: "arm64", Provider: "unsupported", Distribution: "arch"}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "x86_64") {
		t.Fatalf("unexpected Arch Linux ARM result: %v", err)
	}
}

func TestDetectRejectsArchLinuxARM(t *testing.T) {
	info := DetectBytes("linux", "arm64", []byte("ID=arch\n"))
	if info.Provider != "unsupported" || info.DistributionFamily != "arch" {
		t.Fatalf("unexpected Arch Linux ARM detection: %+v", info)
	}
}

func TestDetectAtReadsReleaseFile(t *testing.T) {
	path := t.TempDir() + "/os-release"
	if err := os.WriteFile(path, []byte("ID=ubuntu\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info := DetectAt("linux", "amd64", path)
	if info.Distribution != "ubuntu" || info.Provider != "libvirt" {
		t.Fatalf("unexpected file-backed detection: %+v", info)
	}
}
