//go:build !unix

package collections

import "os"

// fileIno has no answer off unix; see the unix build for what is lost.
func fileIno(os.FileInfo) (uint64, bool) { return 0, false }
