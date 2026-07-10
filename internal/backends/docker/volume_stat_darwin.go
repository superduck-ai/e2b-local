//go:build darwin

package dockerbackend

import (
	"os"
	"syscall"
	"time"

	gateway "e2b-local/internal/gateway"
)

// populateVolumeEntryPlatformStat 从 Darwin stat 结构补齐通用 FileInfo 不提供的属主和时间字段。
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
