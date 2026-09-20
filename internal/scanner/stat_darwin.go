//go:build darwin

package scanner

import (
	"os"
	"syscall"
)

func birthTimeNS(fi os.FileInfo) *int64 {
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	ns := stat.Birthtimespec.Sec*1e9 + int64(stat.Birthtimespec.Nsec)
	return &ns
}
