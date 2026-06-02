//go:build unix

package egresship

import (
	"os"
	"syscall"
)

// inodeOf extracts the underlying inode number. On platforms without a usable
// inode (build tag mismatch) it would return 0; the shipper still works via
// the size-shrink fallback in checkRotation.
func inodeOf(st os.FileInfo) uint64 {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && sys != nil {
		return uint64(sys.Ino)
	}
	return 0
}
