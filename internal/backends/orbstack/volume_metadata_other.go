//go:build !darwin

package orbstackbackend

import "os"

func writeVolumeMetadata(hostDir string, volume RuntimeVolume) error {
	data, err := encodeVolumeMetadata(volume)
	if err != nil {
		return err
	}
	return os.WriteFile(volumeHostMetadataPath(hostDir), data, 0o644)
}

func readVolumeMetadata(hostDir string) (RuntimeVolume, error) {
	return readLegacyVolumeMetadataFile(hostDir)
}

func volumeMetadataStoredAsSidecarFile() bool {
	return true
}
