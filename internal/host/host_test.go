package host

import (
	"strings"
	"testing"

	"github.com/gjpin/agent-os/internal/backend"
)

func TestLinuxAMD64AlwaysSelectsLima(t *testing.T) {
	p, err := Provider(DetectAt("linux", "amd64"), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if lima := p.(backend.Lima); lima.VMType != "qemu" {
		t.Fatalf("Linux driver = %q", lima.VMType)
	}
}

func TestDarwinARM64SelectsVZ(t *testing.T) {
	p, err := Provider(DetectAt("darwin", "arm64"), nil, nil, nil)
	if err != nil || p.(backend.Lima).VMType != "vz" {
		t.Fatalf("provider=%#v err=%v", p, err)
	}
}

func TestUnsupportedHostsFailClearly(t *testing.T) {
	for _, info := range []Info{
		DetectAt("linux", "arm64"), DetectAt("darwin", "amd64"), DetectAt("windows", "amd64"),
	} {
		if _, err := Provider(info, nil, nil, nil); err == nil || !strings.Contains(strings.ToLower(err.Error()), "unsupported") {
			t.Fatalf("host %#v error = %v", info, err)
		}
	}
}
