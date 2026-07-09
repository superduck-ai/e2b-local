//go:build darwin

package dockerbackend

import (
	"os"
	"syscall"
	"time"

	gateway "e2b-local/internal/gateway"
)

func populateVolumeEntryPlatformStat(info os.FileInfo, stat *gateway.VolumeEntryStat) {
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	stat.UID = int(sys.Uid)
	stat.GID = int(sys.Gid)
	stat.Atime = time.Unix(sys.Atimespec.Sec, sys.Atimespec.Nsec)
	stat.Ctime = time.Unix(sys.Ctimespec.Sec, sys.Ctimespec.Nsec)
}
