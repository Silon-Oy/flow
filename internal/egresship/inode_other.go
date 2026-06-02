//go:build !unix

package egresship

import "os"

func inodeOf(st os.FileInfo) uint64 { return 0 }
