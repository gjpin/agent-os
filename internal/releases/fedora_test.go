package releases

import "testing"

func TestFedoraServer44Metadata(t *testing.T) {
	for _, arch := range []string{"x86_64", "aarch64"} {
		image, err := FedoraServer44(arch)
		if err != nil {
			t.Fatal(err)
		}
		if image.Filename == "" || len(image.SHA256) != 64 || image.ChecksumURL == "" {
			t.Fatalf("incomplete image metadata: %+v", image)
		}
	}
	if _, err := FedoraServer44("arm64"); err == nil {
		t.Fatal("expected unsupported architecture")
	}
}
