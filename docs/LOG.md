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

## 2026-11-19 — K1 fix round 3: the code matches docs/STATES.md

- `docs/STATES.md` added: every state machine of the flow (provider, tool, poll loop, key list, refresh on miss,
  start-up and shutdown) with numbered rows and invariants; the packages follow it row by row and the tests are
  named after the rows. Linked from the README and CONTRACT.md.
- Contract: an approved request is delivered on any authorised poll before expiry; approval moves the expiry to at
  least approval plus twice the interval; the interval never exceeds ten times the first; a resumed run keeps the
  pace; `Accept` answers the same for an entry for the life of a consumer; the list consumer refuses plain http.
  The fake provider and a new scenario follow.
- `request`: the background poll keeps the lock; the pending file records the interval and the next poll; a
  resumed `Wait: false` call polls when due; `Wait: false` never waits for another process (`ErrBusy`, or a
  pending from the file); discovery before anything is created; crash leftovers removed by the lock holder; a
  `Notify` panic ends the flow with an error for every caller; `errors.ErrUnsupported`; `Notice.Service`.
- `keylist`: cache before swap; ETag dropped on a failed write; no link followed when emptying; no pull after
  shutdown and a cut pull is not a failure; `Status.Origin`, `Status.ListTime`, a real age for a cache list;
  `Options.AllowInsecureHTTP`; temporary files removed at `Start`.
- Tag `v0.1.0` moved to the new head (never pushed).
- Commits: d41a459, f8b43c2, ce45956, and the commit holding this entry.

## 2026-11-19 — v0.1.1

What changes for a user of the module (no exported API changed):

- `Notice.Service` is now the host of `ServiceURL` when one is set; the override's host only when there is no
  `ServiceURL` (it was the override's host whenever an override was set).
- A resumed `Wait: false` call makes its due poll before it shows the notice: if that poll collects the key, the
  key is returned and no "approve at" notice is printed; otherwise the notice is shown once, as before.
- A `429` whose interval is not smaller than the current one is used as given (at the provider's cap it is
  unchanged); only an unreadable or smaller one adds 2.
- A next poll time cut after a clock was set back is written back to the pending file, so short runs keep polling.
- A `Wait: true` caller that joined a background flow ending with `ErrBusy` starts over instead of returning it.
- The background poll of a `Wait: false` call behind another process resumes only the request it reported.
- Lock, pending and cache files must be owned by the effective user (was the real user).
- A failing cache write is logged once per streak, and the recovery once.
- Clean-up of interrupted writes removes exactly the temporary names `Write` makes for that path.
- Commits: the v0.1.1 commits on top of 9ce1f10; tag `v0.1.1`.
