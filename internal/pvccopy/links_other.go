//go:build !unix

package pvccopy

import "os"

// Link counts cannot be verified on unsupported helper platforms.
func singleLink(os.FileInfo) bool { return false }

func ownership(os.FileInfo) (int, int, bool) { return 0, 0, false }
