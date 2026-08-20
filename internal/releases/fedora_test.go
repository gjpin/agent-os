package releases

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestFedoraCloudBase44Metadata(t *testing.T) {
	for _, arch := range []string{"x86_64", "aarch64"} {
		image, err := FedoraCloudBase44(arch)
		if err != nil {
			t.Fatal(err)
		}
		if image.Filename == "" || len(image.SHA256) != 64 {
			t.Fatalf("incomplete image metadata: %+v", image)
		}
		if image.URL == "" || image.Filename[:len("Fedora-Cloud-Base-Generic-")] != "Fedora-Cloud-Base-Generic-" {
			t.Fatalf("unexpected Fedora Cloud Base image metadata: %+v", image)
		}
	}
	if _, err := FedoraCloudBase44("arm64"); err == nil {
		t.Fatal("expected unsupported architecture")
	}
}

func TestDebianSidDailyMetadata(t *testing.T) {
	for _, tc := range []struct {
		arch, debianArch string
	}{{"x86_64", "amd64"}, {"aarch64", "arm64"}} {
		image, err := DebianSidDaily(tc.arch)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(image.URL, "debian-sid-genericcloud-"+tc.debianArch+"-daily.qcow2") || !strings.HasSuffix(image.ChecksumURL, "/SHA512SUMS") {
			t.Fatalf("unexpected Debian sid image metadata: %+v", image)
		}
	}
	if _, err := DebianSidDaily("riscv64"); err == nil {
		t.Fatal("expected unsupported architecture")
	}
}

type recordingDownloadRunner struct {
	args []string
}

func (r *recordingDownloadRunner) Run(_ context.Context, name string, args []string, _ io.Reader, _ io.Writer, _ io.Writer) error {
	if name != "curl" {
		return errors.New("unexpected executable")
	}
	r.args = append([]string(nil), args...)
	for index, arg := range args {
		if arg == "--output" && index+1 < len(args) {
			return os.WriteFile(args[index+1], []byte("image"), 0o600)
		}
	}
	return errors.New("missing output argument")
}

func TestDownloadVerifiedUsesResumableRetries(t *testing.T) {
	destination := t.TempDir() + "/image.qcow2"
	digest := sha256.Sum256([]byte("image"))
	runner := &recordingDownloadRunner{}
	image := Image{
		URL:    "https://download.fedoraproject.org/pub/fedora/linux/releases/44/Cloud/x86_64/images/image.qcow2",
		SHA256: hex.EncodeToString(digest[:]),
	}

	if err := DownloadVerified(context.Background(), runner, image, destination, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "image" {
		t.Fatalf("downloaded contents = %q, want image", contents)
	}
	for _, want := range []string{"--continue-at", "-", "--retry", "10", "--retry-all-errors", "--retry-max-time", "1800"} {
		if !slices.Contains(runner.args, want) {
			t.Fatalf("curl args %v do not contain %q", runner.args, want)
		}
	}
	if _, err := os.Stat(destination + ".part"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial download still exists: %v", err)
	}
}

type debianDownloadRunner struct {
	image []byte
}

func (r debianDownloadRunner) Run(_ context.Context, name string, args []string, _ io.Reader, _ io.Writer, _ io.Writer) error {
	if name != "curl" {
		return errors.New("unexpected executable")
	}
	var output, source string
	for index, arg := range args {
		if arg == "--output" && index+1 < len(args) {
			output = args[index+1]
		}
	}
	if len(args) > 0 {
		source = args[len(args)-1]
	}
	if strings.HasSuffix(source, "SHA512SUMS") {
		digest := sha512.Sum512(r.image)
		return os.WriteFile(output, []byte(hex.EncodeToString(digest[:])+"  debian-sid-genericcloud-amd64-daily.qcow2\n"), 0o600)
	}
	return os.WriteFile(output, r.image, 0o600)
}

func TestDownloadVerifiedUsesDebianSHA512Manifest(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "debian.qcow2")
	image, err := DebianSidDaily("x86_64")
	if err != nil {
		t.Fatal(err)
	}
	if err := DownloadVerified(context.Background(), debianDownloadRunner{image: []byte("debian-image")}, image, destination, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(destination)
	if err != nil || string(contents) != "debian-image" {
		t.Fatalf("downloaded contents=%q err=%v", contents, err)
	}
}
