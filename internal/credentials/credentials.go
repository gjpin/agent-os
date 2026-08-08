package credentials

import (
	"bufio"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// ValidatePrivateKey checks the metadata and format of a repository private
// key. Encrypted keys are accepted without attempting to retain or discover
// their passphrase; the guest can unlock the original key after boot.
func ValidatePrivateKey(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("repository private-key path is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return errors.New("repository private key does not exist")
		}
		return errors.New("could not stat repository private key")
	}
	if !info.Mode().IsRegular() {
		return errors.New("repository private key must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("repository private key must not be accessible by group or other users")
	}
	publicPath := path + ".pub"
	if _, err := os.Stat(publicPath); err != nil {
		if os.IsNotExist(err) {
			return errors.New("adjacent public key is required")
		}
		return errors.New("could not stat adjacent public key")
	}
	file, err := os.Open(path)
	if err != nil {
		return errors.New("could not open repository private key")
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	validHeader := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "-----BEGIN ") && strings.HasSuffix(line, " PRIVATE KEY-----") {
			validHeader = true
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return errors.New("could not read repository private key metadata")
	}
	if !validHeader {
		return errors.New("repository private key is not a recognized PEM/OpenSSH private key")
	}
	publicInfo, err := os.Stat(publicPath)
	if err != nil || !publicInfo.Mode().IsRegular() {
		return errors.New("adjacent public key must be a regular file")
	}
	if err := validatePublicKey(publicPath); err != nil {
		return err
	}
	return validateCorrespondence(path, publicPath)
}

// ReadPrivateKey validates path and returns the original key bytes unchanged.
// Keeping this operation separate from configuration allows callers that must
// provision a key to do so without ever embedding the host path in a guest
// artifact. In particular, encrypted key bytes are not decrypted here.
func ReadPrivateKey(path string) ([]byte, error) {
	if err := ValidatePrivateKey(path); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("could not read repository private key")
	}
	return data, nil
}

func validatePublicKey(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return errors.New("could not open adjacent public key")
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) >= 2 {
			if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(scanner.Text())); err == nil {
				return nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return errors.New("could not read adjacent public key")
	}
	return errors.New("adjacent public key does not contain an SSH public-key line")
}

func validateCorrespondence(privatePath, publicPath string) error {
	publicData, err := os.ReadFile(publicPath)
	if err != nil {
		return errors.New("could not read adjacent public key")
	}
	publicKey, _, _, _, err := ssh.ParseAuthorizedKey(publicData)
	if err != nil {
		return fmt.Errorf("parse repository public key: %w", err)
	}
	privateData, err := os.ReadFile(privatePath)
	if err != nil {
		return errors.New("could not read repository private key")
	}
	privateKey, err := ssh.ParseRawPrivateKey(privateData)
	if err != nil {
		var passphraseMissing *ssh.PassphraseMissingError
		if errors.As(err, &passphraseMissing) {
			// Encrypted keys are intentionally unlocked only inside the guest
			// after boot; the adjacent public key still remains mandatory.
			return nil
		}
		return fmt.Errorf("parse repository private key: %w", err)
	}
	// ParseRawPrivateKey returns an *ed25519.PrivateKey for OpenSSH Ed25519
	// keys, while NewSignerFromKey expects the value form. Other supported
	// private-key implementations are already returned in the form accepted
	// by the SSH package.
	if key, ok := privateKey.(*ed25519.PrivateKey); ok {
		privateKey = ed25519.PrivateKey(*key)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		return fmt.Errorf("derive repository signer: %w", err)
	}
	if string(signer.PublicKey().Marshal()) != string(publicKey.Marshal()) {
		return errors.New("repository private and public keys do not correspond")
	}
	return nil
}

func GuestKeyPath(path string) string {
	base := filepath.Base(path)
	if base == "." || base == ".." || base == string(filepath.Separator) || base == "" {
		base = "repository-key"
	}
	return filepath.Join("/etc/agent-os/keys", base)
}
