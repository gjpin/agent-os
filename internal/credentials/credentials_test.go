package credentials

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func writeTestKey(t *testing.T, dir, name string, encrypted bool) (string, []byte) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var block *pem.Block
	if encrypted {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(private, "test", []byte("correct horse battery staple"))
	} else {
		block, err = ssh.MarshalPrivateKey(private, "test")
	}
	if err != nil {
		t.Fatal(err)
	}
	privateData := pem.EncodeToMemory(block)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, privateData, 0o600); err != nil {
		t.Fatal(err)
	}
	publicKey, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".pub", ssh.MarshalAuthorizedKey(publicKey), 0o644); err != nil {
		t.Fatal(err)
	}
	return path, privateData
}

func TestReadPrivateKeyReturnsValidatedBytesUnchanged(t *testing.T) {
	dir := t.TempDir()
	path, expected := writeTestKey(t, dir, "id_ed25519", false)

	got, err := ReadPrivateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(expected) {
		t.Fatal("ReadPrivateKey changed the private-key bytes")
	}
	if err := ValidatePrivateKey(path); err != nil {
		t.Fatal(err)
	}
}

func TestReadPrivateKeyPreservesEncryptedKeyForGuestUnlock(t *testing.T) {
	dir := t.TempDir()
	path, expected := writeTestKey(t, dir, "encrypted", true)

	got, err := ReadPrivateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(expected) {
		t.Fatal("encrypted private-key bytes were changed")
	}
	if !strings.Contains(string(got), "BEGIN OPENSSH PRIVATE KEY") {
		t.Fatal("test key is not an OpenSSH private key")
	}
}

func TestReadPrivateKeyRejectsUnsafePermissions(t *testing.T) {
	dir := t.TempDir()
	path, _ := writeTestKey(t, dir, "unsafe", false)
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivateKey(path); err == nil {
		t.Fatal("ReadPrivateKey accepted a group-readable private key")
	}
}

func TestGuestKeyPathDoesNotExposeHostDirectories(t *testing.T) {
	path := "/private/operator/repositories/example/id_ed25519"
	got := GuestKeyPath(path)
	if got != "/etc/agent-os/keys/id_ed25519" {
		t.Fatalf("unexpected guest key path %q", got)
	}
	if strings.Contains(got, "/private/operator/repositories/example") {
		t.Fatal("guest key path contains host directories")
	}
}
