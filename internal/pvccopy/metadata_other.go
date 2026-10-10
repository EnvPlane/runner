//go:build !linux

package pvccopy

import "os"

// Non-Linux CopyLocal is a bytes + numeric POSIX root/entry metadata test helper,
// not a full-filesystem copier. ACL/xattr detection is Linux-runtime-only.
func checkUnsupportedMetadata(*os.File) error { return nil }
