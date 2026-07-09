//go:build !linux && !darwin

package dockerbackend

import (
	"os"

	gateway "e2b-local/internal/gateway"
)

func populateVolumeEntryPlatformStat(info os.FileInfo, stat *gateway.VolumeEntryStat) {
}
