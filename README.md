# keyclient

The client side of two small HTTP contracts for **personal keys**: a tool with no key asks a key provider for one
and a person approves it in a browser; a service pulls the list of key hashes it should accept. One Go module,
standard library only, MIT licensed.

```
tool ──GET /v1/key-request──▶ service        (discovery: where to ask)
tool ──POST request_url, then POST poll_url──▶ provider ◀── a person approves at approve_url
service ──GET list URL (ETag)──▶ provider    (the {name, role, sha256} list; never a key)
```

- The contract: [`docs/CONTRACT.md`](docs/CONTRACT.md) (canonical). OpenAPI: [`spec/key-request.yaml`](spec/key-request.yaml).
  Fixtures: [`testdata/`](testdata/README.md), also embedded as `keyclient.Fixtures()`.
- `request`: what a **tool** calls. `keylist`: what a **service** (daemon) calls. `keyclienttest`: fakes of a
  provider and of a key list for tests.

```sh
go get github.com/resurgentech/keyclient@v0.1.0
```

## A tool: `request.Ensure`

A tool calls `request.Ensure` only when it finds no token where it reads one today. Everything else about the tool
stays as it was.

```go
res, err := request.Ensure(ctx, request.Options{
	ServiceURL: serviceURL,        // the service the tool talks to; discovery is GET <ServiceURL>/v1/key-request
	Override:   os.Getenv("MYTOOL_KEY_REQUEST_URL"), // optional request URL from a flag or env var
	TokenPath:  tokenPath,         // where the tool reads its token; the key is written here
	Role:       "agent",           // the tool's documented default role
	NameHint:   user + "@" + host,
	Tool:       "mytool/1.2.3",
	Notify:     func(n request.Notice) { fmt.Fprintln(os.Stderr, n) },
	Wait:       request.IsTerminal(os.Stderr),
})
var pending *request.ErrPending
switch {
case err == nil:                            // res.Token is the key, now stored at TokenPath; res.Name, res.Role were granted
case errors.Is(err, request.ErrNoProvider): // no provider: report the usual "no token" error
case errors.As(err, &pending):              // no terminal: the notice is printed; exit 2, the next run resumes
default:                                    // denied, expired, refused URL, provider error, ...
}
```

- **With a terminal** (`Wait: true`) the notice goes to stderr, and `Ensure` polls until the request settles,
  stores the key and returns a `Result`: the key and the name and role the approver granted. (When the token was
  already on disk, or another copy of the tool collected it, `Name` and `Role` are empty.)
- **Without a terminal** (`Wait: false`) `Ensure` returns `*request.ErrPending` with the notice at once; a CLI
  prints it and exits 2. The next run resumes the same request from the pending file instead of filing another,
  provided it uses the same `ServiceURL` and `Override`; otherwise the old request is dropped and a new one filed.
- **A long-running server** (an MCP server, say) uses `Wait: false` and keeps the `*ErrPending`: it answers each
  call with the notice until `Ready()` reports the key stored, then reads the token as usual. If `Done()` closes
  without the key (`Err()` says why), the next `Ensure` files a new request.
- A tool that already has a token never makes a request. The service's refresh on miss makes a freshly delivered
  key work on the first call in the usual case; if the service refuses a key that was just delivered, wait the
  service's `refresh_on_miss` (5 s by default) and retry once.
- **One flow per token.** `Ensure` takes an exclusive lock on `TokenPath + ".lock"` before it looks at the token
  or the pending file, and holds it until the flow ends (or until it returns `*ErrPending`). A second copy of the
  tool waits for the lock (bounded by its `ctx`) and then finds the token. Callers in one process share one flow.
  Key requests are supported on Linux, macOS and the BSDs; elsewhere `Ensure` still returns a token already on disk
  and otherwise reports that key requests are not supported.

**Transport.** Plain `http` is refused for the service, the request URL, the poll URL and the approval URL unless
the host is a loopback address or `Options.AllowInsecureHTTP` is set, and a flow that started on `https` never
continues on `http`. The tool trusts the service's discovery answer exactly as much as the service: whoever can
answer for the service can relay the request to the real provider and collect the key while the person sees the
genuine approval page and a matching code. Reaching the service over `https` is what protects the flow.

**On disk** (modes are exact, whatever the umask):

| File | Mode | When |
|---|---|---|
| `TokenPath` | 0600 | Written atomically (temporary file, rename) when the key arrives. |
| its directory | 0700 | Only when `Ensure` creates it; an existing directory is left alone. |
| `TokenPath + ".pending"` | 0600 | While a request is open: request id, poll URL, poll secret, approval URL, code, expiry, and the service URL, override and request URL it was filed under. Removed once the request settles or expires. One that is not a regular file, is readable by group or others, or breaks the field rules is refused. |
| `TokenPath + ".lock"` | 0600 | Empty; holds the lock. Created on the first request and left in place. |

The poll secret and the key appear in no error, notice or log; only in the pending and token files. Whatever is
shown from the provider (the code, the approval URL, messages) is checked against the contract's rules or reduced
to printable characters first.

## A service: `keylist.Source`

A daemon builds one `Source` per configured list, starts it, and checks it **after** its own token file has missed.

```go
src, err := keylist.New(keylist.Options{
	URL:           cfg.PersonalKeys.URL,
	TokenFile:     cfg.PersonalKeys.TokenFile,      // optional pull token
	CacheFile:     cfg.PersonalKeys.CacheFile,
	Refresh:       cfg.PersonalKeys.Refresh,        // 0: keylist.DefaultRefresh (1m)
	RefreshOnMiss: cfg.PersonalKeys.RefreshOnMiss,  // config default keylist.DefaultRefreshOnMiss (5s); 0 turns it off
	// Required: unknown roles are skipped, and the local token file wins.
	Accept: keylist.AcceptRoles([]string{"full", "agent"}, func(e keylist.Entry) bool {
		return ownTokens.HasName(e.Name) || ownTokens.HasHash(e.SHA256)
	}),
})
if err != nil { ... }
src.Start(ctx) // loads the cache, pulls now and every Refresh; the service starts even if the provider is down

// in the bearer middleware:
name, role, ok := ownTokens.Lookup(tok)
if !ok {
	name, role, ok = src.LookupOrRefresh(r.Context(), tok)
}
if !ok { /* 401 */ }

// on a health endpoint:
st := src.Status() // LastSuccess, LastError, Age, Entries, CacheFailing
```

What `keylist` guarantees:

- Lookups hash the presented key with SHA-256 and compare it in constant time with every entry, with no early exit.
- The live list is swapped atomically. A bad body (including duplicate or case-variant member names), a `200` that
  is not `application/json`, a `304` to an unconditional request, a 3xx, 401, 404, 5xx, a network error or a timeout
  keeps the last good list. An empty `200` list revokes every pulled key.
- Each good list is cached atomically with mode 0600 and loaded at start, so a restart with the provider down keeps
  working. The cache is never loaded over a list already pulled in this process. If a cache write fails, `Pull`
  returns an error wrapping `keylist.ErrCacheWrite` (the list is live in memory regardless), `Status().CacheFailing`
  is set, and the older cache is removed or emptied so that a restart never brings back a revoked key. A cache that
  is not a regular file, or that group or others can write, is refused; opening it never blocks.
- There is no staleness cut-off: the last good list is served for as long as the provider is down. That is a
  deliberate availability choice; watch `Status()`.
- **Refresh on miss**: an unknown key triggers one conditional pull and is checked again. At most one such pull
  starts per `RefreshOnMiss` (5 s by default) across all callers; misses that arrive while it is in flight wait for
  it at most 2 s (`keylist.MissWait`), at most 100 of them at once (`keylist.MaxMissWaiters`); every other miss is
  refused at once with no pull. When a miss was refused that way, exactly one more pull starts when the window ends.
  The timer is never moved.
- Log lines name the entry and the reason; never a hash, the pull token or a presented key. Each pull is bounded by
  10 s and 4 MiB; redirects are not followed.

## Tests in an importing repo

```go
p := keyclienttest.NewProvider(t)              // discovery + filing + scripted polls
p.Script(keyclienttest.Pending(), keyclienttest.Approved())
// request.Options{ServiceURL: p.URL(), ...}

l := keyclienttest.NewList(t)                  // a key list with ETags
l.Add("alice@laptop", "agent", "a-key-of-at-least-16-chars")
// keylist.Options{URL: l.URL(), ...}
```

## Build and test

Needs Go 1.25 or newer and [`just`](https://github.com/casey/just).

```sh
just test   # go test -race -count=1 ./...
just vet    # go vet, gofmt, and go.mod has no require
just lint   # vet, plus go mod tidy leaves no diff and the module list is this module alone
```

Versions are plain `v0.x.y` tags. Until `v1`, a minor version may change the API; the contract in
`docs/CONTRACT.md` changes only with a note there.

## License

MIT. See [`LICENSE`](LICENSE).
