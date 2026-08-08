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
	if err := runner.Run(ctx, "curl", []string{"--fail", "--location", "--proto", "=https", "--tlsv1.2", "--retry", "3", "--output", destination, image.URL}, nil, stdout, stderr); err != nil {
		_ = os.Remove(destination)
		return fmt.Errorf("download Fedora image: %w", err)
	}
	file, err := os.Open(destination)
	if err != nil {
		_ = os.Remove(destination)
		return err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(destination)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(destination)
		return closeErr
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(actual, image.SHA256) {
		_ = os.Remove(destination)
		return fmt.Errorf("Fedora image checksum mismatch: got %s", actual)
	}
	return nil
}
