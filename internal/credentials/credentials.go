package credentials

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// ValidatePrivateKey checks only metadata and file format. It never reads key
// material into configuration, state, logs, or generated guest artifacts.
func ValidatePrivateKey(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("repository private-key path is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat repository private key: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("repository private key must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("repository private key %q must not be accessible by group or other users", path)
	}
	publicPath := path + ".pub"
	if _, err := os.Stat(publicPath); err != nil {
		return fmt.Errorf("adjacent public key %q is required: %w", publicPath, err)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open repository private key: %w", err)
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
		return fmt.Errorf("read repository private key metadata: %w", err)
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

func validatePublicKey(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
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
		return err
	}
	return fmt.Errorf("public key %q does not contain an SSH public-key line", path)
}

func validateCorrespondence(privatePath, publicPath string) error {
	publicData, err := os.ReadFile(publicPath)
	if err != nil {
		return err
	}
	publicKey, _, _, _, err := ssh.ParseAuthorizedKey(publicData)
	if err != nil {
		return fmt.Errorf("parse repository public key: %w", err)
	}
	privateData, err := os.ReadFile(privatePath)
	if err != nil {
		return err
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
	derived, err := ssh.NewPublicKey(privateKey)
	if err != nil {
		return fmt.Errorf("derive repository public key: %w", err)
	}
	if string(ssh.Marshal(derived)) != string(ssh.Marshal(publicKey)) {
		return errors.New("repository private and public keys do not correspond")
	}
	return nil
}

func GuestKeyPath(path string) string {
	return filepath.Join("/etc/agent-os/keys", filepath.Base(path))
}
