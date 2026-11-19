# Work log

Append-only history of what shipped from `ROADMAP.md`. FOREMAN appends
an entry per landed step or tranche. Newest at the bottom. Corrections
are appended, never edited in; a hook refuses rewrites.

```
## YYYY-MM-DD — <step id and title>

- What shipped
- Commits: <shas>
```

---

## 2026-11-19 — K1 keyclient: request and keylist packages, contract, fixtures

- `docs/CONTRACT.md` (canonical key-list and key-request contract, with refresh on miss), `spec/key-request.yaml`,
  `testdata/` fixtures (embedded as `keyclient.Fixtures()`), and a test that keeps deployment-specific words out
  of the tree.
- `request.Ensure`: discovery or override, filing, one notice, polling by interval and slow_down, key written
  atomically 0600 (directory 0700 when created), `<token>.pending` 0600 resumed by the next run, `Wait=false`
  returns `*ErrPending` and polls in the background. `request.IsTerminal`.
- `keylist.Source`: ETag/304 pulls, Accept hook, atomic swap, atomic 0600 cache loaded at start, constant-time
  lookups, refresh on miss (one pull per `RefreshOnMiss` window, waits at most 2 s, the timer never moves).
- `keyclienttest.NewProvider` and `keyclienttest.NewList` for tests here and in importing repos.
- Standard library only; gate `just test`, `just vet`, `just lint`. Tag `v0.1.0`, local only.
- Commits: 3e56f25, dd88694, fea3357, bebb27f, 5bf0c31, and the commit holding this entry.

## 2026-11-19 — K1 fix round after the security review (before any publication)

- Contract: tool transport (https except loopback unless allowed, never downgrade, discovery trusted as the
  service), rules for `request_id`, `user_code`, `approve_url`, 24-hour client limit on `expires_in`, resume only
  for the same service and request URL, a last poll at expiry; key list: 304 without `If-None-Match` and non-JSON
  200 are failures, strict member names, cache never loaded over a pulled list, stale cache removed on a failed
  write, untrusted cache refused, waiter bound and one trailing pull per throttled window, retry-once advice.
- `request`: `Ensure` returns `Result` (token, granted name and role); `Options.AllowInsecureHTTP`; validated
  provider text; sanitise-then-redact; `<token>.lock` flock and an in-process table (one flow per token); pending
  file bound to its service and override and removed only by its own request; real `IsTerminal`.
- `keylist`: `Accept` required, `AcceptRoles`, `Status`, `ErrCacheWrite`, `MaxMissWaiters`, trailing pull.
- `internal/atomicfile`: never chmods a directory it did not create, nor through a symlink.
- Tag `v0.1.0` moved to the new head (it had never been pushed).
- Commits: dc58b30, 95f764f, 6fafe3b, ef6b0d3, and the commit holding this entry.
