// Package keylist is the service side of the key-list contract (docs/CONTRACT.md,
// "Key list"): a [Source] pulls the list of SHA-256 hashes of the personal keys
// a service should accept, and checks presented keys against it.
//
// A daemon calls, in order:
//
//  1. [New] with [Options]: URL, TokenFile, CacheFile, Refresh, RefreshOnMiss
//     (default [DefaultRefreshOnMiss]) and an Accept hook that refuses unknown
//     roles and clashes with its own tokens.
//  2. [Source.Start] once. It loads the cache, then pulls at once and every
//     Refresh. The service starts even when the provider is down.
//  3. In its authentication, after its own tokens have missed,
//     [Source.LookupOrRefresh]; a miss is a 401.
//
// Guarantees:
//
//   - A presented key is hashed and compared in constant time with every
//     entry, with no early exit. An empty key never matches.
//   - No log line holds a hash, the pull token or a presented key; skipped
//     entries are logged with their name and reason. Pull errors never hold the
//     token, a hash or URL userinfo.
//   - The cache is written atomically with mode 0600; a restart with the
//     provider down keeps the last good list. A failed pull, a bad body, a 404
//     or a 3xx keeps the last good list; an empty list revokes every key.
//   - Refresh on miss: a key the list does not hold triggers one conditional
//     pull, joined by concurrent misses for at most [MissWait] (2 s); at most
//     one such pull starts per RefreshOnMiss across all callers, and a miss
//     inside that window returns at once. The timer is never moved. Each pull
//     is bounded by [PullTimeout] (10 s) and reads at most [MaxBody] (4 MiB).
package keylist
