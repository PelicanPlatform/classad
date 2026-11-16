//go:build unix

package collections

import (
	"os"
	"syscall"
)

// fileIno returns the inode number behind a FileInfo. ok is false if the platform does
// not expose one, in which case a dictionary file that is REPLACED rather than removed
// cannot be told from the original by identity alone, and only its contents give it away
// (see dictPin.verify).
func fileIno(fi os.FileInfo) (uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Ino, true
}
