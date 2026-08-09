package backend

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLibvirtSecurityModelForDistribution(t *testing.T) {
	tests := []struct {
		id   string
		want string
	}{
		{id: "arch", want: "dac"},
		{id: "ubuntu", want: "apparmor"},
		{id: "fedora", want: "selinux"},
	}
	for _, test := range tests {
		t.Run(test.id, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "os-release")
			if err := os.WriteFile(path, []byte("NAME=test\nID=\""+test.id+"\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := libvirtSecurityModelAt(path); got != test.want {
				t.Fatalf("unexpected security model: got %q want %q", got, test.want)
			}
		})
	}
}
