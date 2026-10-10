//go:build linux

package pvccopy

import (
	"fmt"
	"os"
	"syscall"
)

// Ask only for the size of the name list on the already-opened inode. A single
// visible xattr (including POSIX ACL or SELinux metadata) is unsupported. Never
// allocate/read values, follow arbitrary paths, or add CAP_SYS_ADMIN to expose
// hidden trusted.* attributes; the documented subset makes no such guarantee.
func checkUnsupportedMetadata(f *os.File) error {
	n, _, errno := syscall.Syscall(syscall.SYS_FLISTXATTR, f.Fd(), 0, 0)
	return attributeResult(n, errno)
}

func attributeResult(n uintptr, errno syscall.Errno) error {
	if errno == syscall.ENOTSUP {
		return nil
	} // Filesystem has no xattr support.
	if errno != 0 {
		return fmt.Errorf("%w: cannot enumerate extended attributes: %w", ErrUnsafe, errno)
	}
	if n != 0 {
		return fmt.Errorf("%w: unsupported extended attributes or ACL", ErrUnsafe)
	}
	return nil
}
