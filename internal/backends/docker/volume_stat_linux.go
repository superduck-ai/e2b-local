//go:build linux

package dockerbackend

import (
	"os"
	"syscall"
	"time"

	gateway "e2b-local/internal/gateway"
)

// populateVolumeEntryPlatformStat 从 Linux stat 结构补齐通用 FileInfo 不提供的属主和时间字段。
func populateVolumeEntryPlatformStat(info os.FileInfo, stat *gateway.VolumeEntryStat) {
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	stat.UID = int(sys.Uid)
	stat.GID = int(sys.Gid)
	stat.Atime = time.Unix(sys.Atim.Sec, sys.Atim.Nsec)
	stat.Ctime = time.Unix(sys.Ctim.Sec, sys.Ctim.Nsec)
}
