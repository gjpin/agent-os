package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"

	"github.com/gjpin/agent-os/internal/execx"
	"github.com/gjpin/agent-os/internal/profile"
)

type profileInfo struct {
	Metadata profile.Metadata
	Store    profile.Store
	DiskPath string
}

func profileFor(spec Spec, provider string) (profileInfo, error) {
	if err := spec.Config.Validate(); err != nil {
		return profileInfo{}, err
	}
	store := profile.NewStore(spec.Config.StateDir)
	diskID := profile.DiskID(spec.Config.VMName)
	diskPath, err := store.DiskPath(spec.Config.VMName)
	if err != nil {
		return profileInfo{}, err
	}
	return profileInfo{
		Metadata: profile.Metadata{
			SchemaVersion: profile.SchemaVersion,
			Provider:      provider,
			DiskID:        diskID,
			SizeGiB:       spec.Config.ProfileDiskGiB,
			Filesystem:    profile.Filesystem,
			Label:         profile.DiskLabel(provider, diskID),
		},
		Store:    store,
		DiskPath: diskPath,
	}, nil
}

func loadProfile(spec Spec, provider string) (profileInfo, profile.Metadata, bool, error) {
	info, err := profileFor(spec, provider)
	if err != nil {
		return profileInfo{}, profile.Metadata{}, false, err
	}
	existing, err := info.Store.Load(spec.Config.VMName)
	if err == nil {
		if existing.Provider != info.Metadata.Provider {
			return profileInfo{}, profile.Metadata{}, false, fmt.Errorf("profile disk belongs to provider %q, refusing to use it with provider %q", existing.Provider, info.Metadata.Provider)
		}
		if existing.DiskID != info.Metadata.DiskID || existing.Filesystem != info.Metadata.Filesystem || existing.Label != info.Metadata.Label {
			return profileInfo{}, profile.Metadata{}, false, errors.New("profile metadata does not match the deterministic disk identity or filesystem; refusing to overwrite data")
		}
		return info, existing, true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return profileInfo{}, profile.Metadata{}, false, err
	}
	return info, info.Metadata, false, nil
}

func saveProfile(info profileInfo, name string, value profile.Metadata) error {
	if err := info.Store.Save(name, value); err != nil {
		return fmt.Errorf("save profile metadata: %w", err)
	}
	return nil
}

func ensureLibvirtProfile(ctx context.Context, runner execx.Runner, out, errOut io.Writer, spec Spec) error {
	info, metadata, found, err := loadProfile(spec, "libvirt")
	if err != nil {
		return err
	}
	dir := filepath.Dir(info.DiskPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create profile directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	diskInfo, statErr := os.Lstat(info.DiskPath)
	diskExists := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("inspect profile disk: %w", statErr)
	}
	if !found && diskExists {
		return errors.New("profile disk exists without trusted metadata; refusing to overwrite it")
	}
	if found && !diskExists {
		return errors.New("profile metadata exists but its disk is missing; refusing to create a replacement")
	}
	if diskExists && (diskInfo.Mode()&os.ModeSymlink != 0 || !diskInfo.Mode().IsRegular()) {
		return errors.New("profile disk is not a regular file; refusing to use it")
	}
	if !diskExists {
		if err := command(runner, ctx, "qemu-img", []string{"create", "-f", "qcow2", "-o", "size=" + strconv.Itoa(spec.Config.ProfileDiskGiB) + "G", info.DiskPath}, nil, out, errOut); err != nil {
			return fmt.Errorf("create profile disk: %w", err)
		}
		if err := os.Chmod(info.DiskPath, 0o600); err != nil {
			return err
		}
		return saveProfile(info, spec.Config.VMName, metadata)
	}

	actual, err := qemuVirtualSize(ctx, runner, info.DiskPath, errOut)
	if err != nil {
		return err
	}
	requiredGiB := spec.Config.ProfileDiskGiB
	if metadata.SizeGiB > requiredGiB {
		requiredGiB = metadata.SizeGiB
	}
	actualGiB := int(math.Ceil(float64(actual) / float64(1<<30)))
	if actual < int64(requiredGiB)<<30 {
		if err := command(runner, ctx, "qemu-img", []string{"resize", info.DiskPath, strconv.Itoa(requiredGiB) + "G"}, nil, out, errOut); err != nil {
			return fmt.Errorf("grow profile disk: %w", err)
		}
		actualGiB = requiredGiB
	}
	if actualGiB > metadata.SizeGiB {
		metadata.SizeGiB = actualGiB
	}
	if err := os.Chmod(info.DiskPath, 0o600); err != nil {
		return err
	}
	return saveProfile(info, spec.Config.VMName, metadata)
}

func qemuVirtualSize(ctx context.Context, runner execx.Runner, path string, errOut io.Writer) (int64, error) {
	var output bytes.Buffer
	if err := command(runner, ctx, "qemu-img", []string{"info", "--output=json", path}, nil, &output, errOut); err != nil {
		return 0, fmt.Errorf("inspect profile disk size: %w", err)
	}
	var info struct {
		VirtualSize int64 `json:"virtual-size"`
	}
	if err := json.Unmarshal(output.Bytes(), &info); err != nil || info.VirtualSize < 1 {
		if err == nil {
			err = errors.New("missing virtual-size")
		}
		return 0, fmt.Errorf("invalid qemu-img size output: %w", err)
	}
	return info.VirtualSize, nil
}

func ensureLimaProfile(ctx context.Context, runner execx.Runner, out, errOut io.Writer, spec Spec) error {
	info, metadata, found, err := loadProfile(spec, "lima")
	if err != nil {
		return err
	}
	present, size, err := limaDiskDetails(ctx, runner, metadata.DiskID, errOut)
	if err != nil {
		return err
	}
	if !found && present {
		return errors.New("Lima profile disk exists without trusted metadata; refusing to reuse it")
	}
	if found && !present {
		return errors.New("profile metadata exists but the Lima disk is missing; refusing to create a replacement")
	}
	if !present {
		if err := command(runner, ctx, "limactl", []string{"disk", "create", metadata.DiskID, "--size", strconv.Itoa(spec.Config.ProfileDiskGiB) + "GiB", "--format", "qcow2"}, nil, out, errOut); err != nil {
			return fmt.Errorf("create Lima profile disk: %w", err)
		}
		return saveProfile(info, spec.Config.VMName, metadata)
	}
	requiredGiB := spec.Config.ProfileDiskGiB
	if metadata.SizeGiB > requiredGiB {
		requiredGiB = metadata.SizeGiB
	}
	if size > 0 && size < requiredGiB {
		if err := command(runner, ctx, "limactl", []string{"disk", "resize", metadata.DiskID, "--size", strconv.Itoa(requiredGiB) + "GiB"}, nil, out, errOut); err != nil {
			return fmt.Errorf("grow Lima profile disk: %w", err)
		}
		size = requiredGiB
	}
	if size > metadata.SizeGiB {
		metadata.SizeGiB = size
	}
	return saveProfile(info, spec.Config.VMName, metadata)
}

func limaDiskDetails(ctx context.Context, runner execx.Runner, diskID string, errOut io.Writer) (bool, int, error) {
	var output bytes.Buffer
	err := command(runner, ctx, "limactl", []string{"disk", "list", "--json"}, nil, &output, errOut)
	if err != nil {
		return false, 0, fmt.Errorf("inspect Lima profile disk: %w", err)
	}
	disks, err := decodeLimaDisks(output.Bytes())
	if err != nil {
		return false, 0, fmt.Errorf("decode Lima profile disk list: %w", err)
	}
	for _, disk := range disks {
		if disk.Name != diskID {
			continue
		}
		return true, limaDiskSizeGiB(disk.Size), nil
	}
	return false, 0, nil
}

type limaDisk struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

func decodeLimaDisks(data []byte) ([]limaDisk, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, nil
	}
	if data[0] == '[' {
		var disks []limaDisk
		if err := json.Unmarshal(data, &disks); err != nil {
			return nil, err
		}
		return disks, nil
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	var disks []limaDisk
	for {
		var disk limaDisk
		err := decoder.Decode(&disk)
		if errors.Is(err, io.EOF) {
			return disks, nil
		}
		if err != nil {
			return nil, err
		}
		disks = append(disks, disk)
	}
}

func limaDiskSizeGiB(size int64) int {
	if size <= 0 {
		return 0
	}
	return int(math.Ceil(float64(size) / float64(1<<30)))
}

func purgeProfileMetadata(spec Spec) error {
	return profile.NewStore(spec.Config.StateDir).Delete(spec.Config.VMName)
}
