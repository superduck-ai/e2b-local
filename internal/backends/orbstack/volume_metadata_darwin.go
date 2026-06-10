//go:build darwin

package orbstackbackend

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const (
	volumeMetadataXAttrName         = "com.e2b.local.volume-meta"
	previousVolumeMetadataXAttrName = "com.e2b.volume-meta"
)

func writeVolumeMetadata(hostDir string, volume RuntimeVolume) error {
	data, err := encodeVolumeMetadata(volume)
	if err != nil {
		return err
	}

	if err := unix.Setxattr(hostDir, volumeMetadataXAttrName, data, 0); err != nil {
		return fmt.Errorf("set volume metadata xattr %s: %w", volume.VolumeID, err)
	}
	if err := unix.Removexattr(hostDir, previousVolumeMetadataXAttrName); err != nil && !isMissingVolumeMetadataXAttr(err) {
		return fmt.Errorf("remove legacy volume metadata xattr %s: %w", volume.VolumeID, err)
	}
	if err := os.Remove(volumeHostMetadataPath(hostDir)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove legacy volume metadata %s: %w", volume.VolumeID, err)
	}
	return nil
}

func readVolumeMetadata(hostDir string) (RuntimeVolume, error) {
	data, err := readVolumeMetadataXAttr(hostDir, volumeMetadataXAttrName)
	if err == nil {
		return decodeVolumeMetadata(filepath.Base(hostDir), data)
	}
	if !isMissingVolumeMetadataXAttr(err) {
		return RuntimeVolume{}, err
	}

	data, err = readVolumeMetadataXAttr(hostDir, previousVolumeMetadataXAttrName)
	if err == nil {
		volume, err := decodeVolumeMetadata(filepath.Base(hostDir), data)
		if err != nil {
			return RuntimeVolume{}, err
		}
		if err := writeVolumeMetadata(hostDir, volume); err != nil {
			return RuntimeVolume{}, fmt.Errorf("migrate legacy volume metadata xattr %s: %w", filepath.Base(hostDir), err)
		}
		return volume, nil
	}
	if !isMissingVolumeMetadataXAttr(err) {
		return RuntimeVolume{}, err
	}

	volume, err := readLegacyVolumeMetadataFile(hostDir)
	if err != nil {
		return RuntimeVolume{}, err
	}
	if err := writeVolumeMetadata(hostDir, volume); err != nil {
		return RuntimeVolume{}, fmt.Errorf("migrate volume metadata %s to xattr: %w", filepath.Base(hostDir), err)
	}
	return volume, nil
}

func readVolumeMetadataXAttr(path string, attr string) ([]byte, error) {
	size, err := unix.Getxattr(path, attr, nil)
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return []byte{}, nil
	}

	data := make([]byte, size)
	readSize, err := unix.Getxattr(path, attr, data)
	if err != nil {
		return nil, err
	}
	return data[:readSize], nil
}

func isMissingVolumeMetadataXAttr(err error) bool {
	return errors.Is(err, unix.ENOATTR) || errors.Is(err, unix.ENODATA)
}

func volumeMetadataStoredAsSidecarFile() bool {
	return false
}
