//go:build linux

package profiles

import (
	"os"
	"syscall"
)

func sourceChangeTimeNS(info os.FileInfo) int64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return stat.Ctim.Sec*1_000_000_000 + stat.Ctim.Nsec
}
