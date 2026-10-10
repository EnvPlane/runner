//go:build unix

package pvccopy

import (
	"os"
	"syscall"
)

func singleLink(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Nlink == 1
}

func ownership(info os.FileInfo) (int, int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint64(st.Uid) > 1<<31-1 || uint64(st.Gid) > 1<<31-1 {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
