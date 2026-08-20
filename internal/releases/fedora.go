package releases

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gjpin/agent-os/internal/execx"
)

const (
	FedoraCloudBase = "https://download.fedoraproject.org/pub/fedora/linux/releases/44/Cloud"
)

type Image struct {
	Architecture string
	URL          string
	Filename     string
	SHA256       string
	SHA512       string
	ChecksumURL  string
}

const DebianSidCloudBase = "https://cloud.debian.org/images/cloud/sid/daily/latest"

func DebianSidDaily(architecture string) (Image, error) {
	var debianArchitecture string
	switch architecture {
	case "x86_64":
		debianArchitecture = "amd64"
	case "aarch64":
		debianArchitecture = "arm64"
	default:
		return Image{}, fmt.Errorf("Debian sid genericcloud is not published for architecture %q", architecture)
	}
	filename := fmt.Sprintf("debian-sid-genericcloud-%s-daily.qcow2", debianArchitecture)
	return Image{
		Architecture: architecture,
		URL:          DebianSidCloudBase + "/" + filename,
		Filename:     filename,
		ChecksumURL:  DebianSidCloudBase + "/SHA512SUMS",
	}, nil
}

func FedoraCloudBase44(architecture string) (Image, error) {
	var filename string
	var checksum string
	switch architecture {
	case "x86_64":
		filename = "Fedora-Cloud-Base-Generic-44-1.7.x86_64.qcow2"
		checksum = "28680fe5b371a5a82ebf43a31926e086a168e59949d03969c5093e7071f90b7f"
	case "aarch64":
		filename = "Fedora-Cloud-Base-Generic-44-1.7.aarch64.qcow2"
		checksum = "55c60a3b80d3616a08705afd0459e75fe9f03c54aba7a46e4002a41a72fa0d5b"
	default:
		return Image{}, fmt.Errorf("Fedora Cloud Base 44 is not published for architecture %q", architecture)
	}
	return Image{
		Architecture: architecture,
		URL:          FedoraCloudBase + "/" + architecture + "/images/" + filename,
		Filename:     filename,
		SHA256:       checksum,
	}, nil
}

func DownloadVerified(ctx context.Context, runner execx.Runner, image Image, destination string, stdout, stderr io.Writer) error {
	if runner == nil {
		return errors.New("download runner is nil")
	}
	trustedURL := strings.HasPrefix(image.URL, "https://download.fedoraproject.org/") || strings.HasPrefix(image.URL, DebianSidCloudBase+"/")
	if !trustedURL || (len(image.SHA256) != sha256.Size*2 && image.ChecksumURL == "") {
		return errors.New("image metadata is not a trusted HTTPS artifact")
	}
	if destination == "" {
		return errors.New("image destination is empty")
	}
	if err := os.Remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	partial := destination + ".part"
	for attempt := 0; attempt < 2; attempt++ {
		expected256 := image.SHA256
		expected512 := image.SHA512
		if image.ChecksumURL != "" {
			checksum, err := fetchImageChecksum(ctx, runner, image, destination+".checksums", stdout, stderr)
			if err != nil {
				return err
			}
			expected512 = checksum
		}
		if err := runner.Run(ctx, "curl", []string{
			"--fail", "--location", "--proto", "=https", "--tlsv1.2",
			"--connect-timeout", "15", "--max-time", "1800",
			"--retry", "10", "--retry-delay", "5", "--retry-max-time", "1800", "--retry-all-errors",
			"--continue-at", "-", "--output", partial, image.URL,
		}, nil, stdout, stderr); err != nil {
			return fmt.Errorf("download guest image: %w", err)
		}
		actual, err := imageDigest(partial, expected512 != "")
		if err != nil {
			return err
		}
		expected := expected256
		if expected512 != "" {
			expected = expected512
		}
		if strings.EqualFold(actual, expected) {
			break
		}
		_ = os.Remove(partial)
		if attempt == 1 || image.ChecksumURL == "" {
			return fmt.Errorf("guest image checksum mismatch: got %s", actual)
		}
	}
	if err := os.Rename(partial, destination); err != nil {
		_ = os.Remove(partial)
		return err
	}
	return nil
}

func fetchImageChecksum(ctx context.Context, runner execx.Runner, image Image, path string, stdout, stderr io.Writer) (string, error) {
	defer os.Remove(path)
	if !strings.HasPrefix(image.ChecksumURL, DebianSidCloudBase+"/") {
		return "", errors.New("untrusted guest image checksum URL")
	}
	if err := runner.Run(ctx, "curl", []string{"--fail", "--location", "--proto", "=https", "--tlsv1.2", "--retry", "10", "--output", path, image.ChecksumURL}, nil, stdout, stderr); err != nil {
		return "", fmt.Errorf("download guest image checksums: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == image.Filename && len(fields[0]) == sha512.Size*2 {
			if _, err := hex.DecodeString(fields[0]); err == nil {
				return fields[0], nil
			}
		}
	}
	return "", fmt.Errorf("checksum manifest does not contain %q", image.Filename)
}

func imageDigest(path string, useSHA512 bool) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if useSHA512 {
		hash := sha512.New()
		if _, err := io.Copy(hash, file); err != nil {
			return "", err
		}
		return hex.EncodeToString(hash.Sum(nil)), nil
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
