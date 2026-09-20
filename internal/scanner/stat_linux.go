//go:build linux

package scanner

import "os"

// Linux's Ctim is inode change time, not file birth time.
// Some newer kernels expose Btime but it's not universally available.
// We return nil (stored as NULL) rather than silently storing incorrect data.
func birthTimeNS(_ os.FileInfo) *int64 {
	return nil
}
