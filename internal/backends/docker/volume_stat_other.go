//go:build !linux && !darwin

package dockerbackend

import (
	"os"

	gateway "e2b-local/internal/gateway"
)

// populateVolumeEntryPlatformStat 在未知平台保留调用方设置的 mtime 回退值与 UID/GID 零值。
func populateVolumeEntryPlatformStat(info os.FileInfo, stat *gateway.VolumeEntryStat) {
}
