# keyclient

**keyclient**: the client side of two small HTTP contracts for personal keys. A tool with no key asks a
key provider for one and a person approves it (`request`); a service pulls the list of key hashes it should
accept (`keylist`). One Go module, standard library only.

- `request/`: the tool side (`request.Ensure`). `keylist/`: the service side (`keylist.Source`).
- `keyclienttest/`: fakes of a provider and of a list, for tests here and in importing repos.
- `docs/CONTRACT.md`: the canonical contract text. `spec/key-request.yaml`: the OpenAPI document.
  `testdata/`: the canonical fixtures (embedded, see `keyclient.Fixtures`).
- Tooling: plain `just` at the root (`just test`, `just vet`, `just lint`).

## Roles

The default role of a session is **XO**: a request in chat becomes a
`ROADMAP.md` step, not an edit. XO drafts a spawn prompt, shows it, and
spawns **ARCHITECT** (plans into `ROADMAP.md`) at once, or waits for
approval and then spawns **FOREMAN** (builds from it); either runs with
a clean context, and XO reviews the report. When the user asks to run
a request unattended (`/orch run`), the roadmap is accepted in advance
and XO chains the two without stopping. Answer directly only when no
code change is needed and the context is in hand, or when the user
says to just do it.

## Hard rules

1. **FOREMAN builds only from `ROADMAP.md`.** No scope from chat.
2. **`docs/LOG.md` is append-only.**
3. **The gate stays green** for packages you touch before a commit.
   Default gate: `just test`, `just vet` and `just lint` at the root (`.orch.json`).
4. **Every built step leaves a report** in `docs/reports/`, with its
   assumptions: context, question, decision, alternatives, confidence.
5. **Secrets** never go in the repo, the roadmap, the reports or the
   log.
6. **Contract first**: change `docs/CONTRACT.md`, `spec/key-request.yaml` and `testdata/` first, then the code
   and its tests together.
7. **Standard library only.** `go.mod` has no `require`; `just vet` fails otherwise.
8. **Public and generic.** No deployment's host names, URLs, product or organisation names in code, docs,
   fixtures or commit messages. A test (`publictext_test.go`) checks the tree.
9. **Tags are permanent once pushed.** Plain `v0.x.y`; `go mod tidy` leaves no diff in the tagged commit.

## Where to look

| Need | Where |
|---|---|
| The queue | `ROADMAP.md` (local, gitignored; `/orch init` makes one) |
| What shipped | `docs/LOG.md` |
| What was done, and the judgment calls | `docs/reports/` (local, gitignored): one file per built step, recap plus assumptions |
| The contract | `docs/CONTRACT.md`, `spec/key-request.yaml`, `testdata/` |
| Roles and process | the orch plugin (`/orch`) |
| Agent definitions | `architect`, `foreman`, `implementer`, `recon` (plugin) |
