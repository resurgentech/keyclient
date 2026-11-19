//go:build unix

package keylist

import "io/fs"

// cacheWritableByOthers reports whether group or others can write the cache.
func cacheWritableByOthers(m fs.FileMode) bool { return m&0o022 != 0 }
