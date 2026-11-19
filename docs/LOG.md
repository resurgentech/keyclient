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
