//go:build !unix

package keylist

import "io/fs"

// cacheWritableByOthers is false where the file mode says nothing about who can
// write (permission bits are not meaningful there).
func cacheWritableByOthers(fs.FileMode) bool { return false }
