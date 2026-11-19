// Package keylist is the service side of the key-list contract (docs/CONTRACT.md,
// "Key list"): a [Source] pulls the list of SHA-256 hashes of the personal keys
// a service should accept, and checks presented keys against it.
//
// A daemon calls, in order:
//
//  1. [New] with [Options]: URL, TokenFile, CacheFile, Refresh, RefreshOnMiss
//     (default [DefaultRefreshOnMiss]) and Accept, which is required:
//     [AcceptRoles] refuses unknown roles and clashes with the daemon's own
//     tokens, as the contract says. Accept must answer the same for the same
//     entry for the life of the Source; a daemon whose tokens or roles change
//     restarts it. A plain http URL is refused unless its host is a loopback
//     address or [Options.AllowInsecureHTTP] is set.
//  2. [Source.Start] once. It loads the cache, then pulls at once and every
//     Refresh. The service starts even when the provider is down. When the
//     context ends no pull starts again; lookups go on from the list in memory.
//  3. In its authentication, after its own tokens have missed,
//     [Source.LookupOrRefresh]; a miss is a 401.
//
// [Source.Status] reports the health: the last good pull, the latest error, where
// the live list came from ([Origin]: none, the cache or the provider) and its
// age (for a cache, the age of the cache file), the entry count and whether the
// cache is failing. There is no staleness cut-off: while the provider is down the
// last good list is served, a deliberate availability choice.
//
// Guarantees:
//
//   - A presented key is hashed and compared in constant time with every
//     entry, with no early exit. An empty key never matches.
//   - No log line holds a hash, the pull token or a presented key; skipped
//     entries are logged with their name and reason. Pull errors never hold the
//     token, a hash or URL userinfo.
//   - The cache is written atomically with mode 0600, and before the new list
//     goes live, so it is never older than a list that was live; a restart with
//     the provider down keeps the last good list. A failed pull, a bad body, a
//     404 or a 3xx keeps the last good list; an empty list revokes every key.
//   - A cache write that fails removes (or empties, never through a symlink and
//     only a regular file) the older cache, leaves the version marker unset so
//     the next pull is a full 200 that writes the cache again, and makes
//     [Source.Pull] return an error wrapping [ErrCacheWrite], so a restart never
//     loads a list older than one already applied. The cache is not loaded over
//     a list pulled in this process, and is ignored unless it is a regular file
//     that group and others cannot write and the current user owns (it is opened
//     without blocking). Start removes the temporary files of an interrupted
//     write.
//   - A 200 must be application/json, a 304 needs an If-None-Match that was
//     sent, and the body is one JSON object: a repeated member name, a member
//     that differs from a known name only in letter case, or trailing data is a
//     bad body.
//   - Refresh on miss: a key the list does not hold triggers one conditional
//     pull, joined by concurrent misses for at most [MissWait] (2 s); at most
//     one such pull starts per RefreshOnMiss across all callers, and a miss
//     inside that window returns at once; at most [MaxMissWaiters] callers
//     wait, the rest return at once. A miss refused a pull gets exactly one
//     trailing pull when its window ends (none is armed when the window is
//     already over). The timer is never moved. Each pull is bounded by
//     [PullTimeout] (10 s) and reads at most [MaxBody] (4 MiB).
//   - After the context given to Start has ended, a miss is answered from the
//     list at once, nothing is logged, and a pull cut short by the shutdown is
//     not a failure.
package keylist
