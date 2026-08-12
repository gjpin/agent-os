package releases

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gjpin/agent-os/internal/execx"
)

const (
	FedoraServerVersion = "44-1.7"
	FedoraDownloadBase  = "https://download.fedoraproject.org/pub/fedora/linux/releases/44/Server"
	FedoraCloudBase     = "https://download.fedoraproject.org/pub/fedora/linux/releases/44/Cloud"
)

type Image struct {
	Architecture string
	URL          string
	Filename     string
	SHA256       string
	ChecksumURL  string
}

func FedoraServer44(architecture string) (Image, error) {
	var filename string
	switch architecture {
	case "x86_64":
		filename = "Fedora-Server-Guest-Generic-44-1.7.x86_64.qcow2"
	case "aarch64":
		filename = "Fedora-Server-Guest-Generic-44-1.7.aarch64.qcow2"
	default:
		return Image{}, fmt.Errorf("Fedora Server 44 is not published for architecture %q", architecture)
	}
	checksums := map[string]string{
		"x86_64":  "446c01f71e3c6cd3889af66fec927b8d1160b8e0744243d7861c2b8b2ddd3f0e",
		"aarch64": "c2320b52a25bd961b277361b74edbeb49e8a5b45b5a1df8481865457fb577935",
	}
	return Image{
		Architecture: architecture,
		URL:          FedoraDownloadBase + "/" + architecture + "/images/" + filename,
		Filename:     filename,
		SHA256:       checksums[architecture],
		ChecksumURL:  FedoraDownloadBase + "/" + architecture + "/images/Fedora-Server-44-1.7-" + architecture + "-CHECKSUM",
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
		ChecksumURL:  FedoraCloudBase + "/" + architecture + "/images/Fedora-Cloud-44-1.7-" + architecture + "-CHECKSUM",
	}, nil
}

func DownloadVerified(ctx context.Context, runner execx.Runner, image Image, destination string, stdout, stderr io.Writer) error {
	if runner == nil {
		return errors.New("download runner is nil")
	}
	if !strings.HasPrefix(image.URL, "https://download.fedoraproject.org/") || len(image.SHA256) != sha256.Size*2 {
		return errors.New("image metadata is not a pinned Fedora HTTPS artifact")
	}
	if destination == "" {
		return errors.New("image destination is empty")
	}
	if err := os.Remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	partial := destination + ".part"
	if err := runner.Run(ctx, "curl", []string{
		"--fail", "--location", "--proto", "=https", "--tlsv1.2",
		"--connect-timeout", "15", "--max-time", "1800",
		"--retry", "10", "--retry-delay", "5", "--retry-max-time", "1800", "--retry-all-errors",
		"--continue-at", "-", "--output", partial, image.URL,
	}, nil, stdout, stderr); err != nil {
		return fmt.Errorf("download Fedora image: %w", err)
	}
	file, err := os.Open(partial)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(actual, image.SHA256) {
		_ = os.Remove(partial)
		return fmt.Errorf("Fedora image checksum mismatch: got %s", actual)
	}
	if err := os.Rename(partial, destination); err != nil {
		_ = os.Remove(partial)
		return err
	}
	return nil
}
