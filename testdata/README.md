# Fixtures

The HTTP exchanges that the key list and key request contracts are tested with
([`docs/CONTRACT.md`](../docs/CONTRACT.md)). These files are the canonical
copy: a provider or a consumer in another repository tests against them, either
by copying this directory or through `keyclient.Fixtures()`, which embeds it.
`keyclienttest` plays them. They name no host, product or secret.

## Format

Each `*.json` file, except `keyrequest/scenarios.json`, is one exchange written
as an OpenAPI Example Object:

```json
{
  "summary": "Poll: pending",
  "description": "...",
  "x-status": 202,
  "x-headers": {"Content-Type": "application/json"},
  "value": {"status": "pending", "interval": 3}
}
```

| Member | Meaning |
|---|---|
| `summary`, `description` | Text for a reader. |
| `x-status` | The HTTP status of the reply. Absent in a request body (`keyrequest/request.json`). |
| `x-headers` | The headers a test asserts or sends. Header names are case-insensitive. |
| `value` | The body, as JSON. Absent when there is no body (`keylist/not-modified.json`, `keyrequest/discovery-none.json`). |

[`spec/key-request.yaml`](../spec/key-request.yaml) references a file with
`examples: <name>: $ref: '../testdata/...json'`, so an OpenAPI lint checks
`value` against the schema of that response.

## Values

- URLs use the origin `https://provider.example`. A test harness replaces it with
  the base URL of its own server.
- Every secret and key is a fixed, plainly fake value. The `request_id` (22
  characters) and `poll_secret` (43 characters) have the lengths and alphabet
  of a real provider's; the key is opaque and has no provider prefix.
- Timing in the fixtures: the interval is 3 and the request expires in 600
  seconds. `poll-slow-down.json` carries 5, which is 3 plus 2.
- `ETag` values are fixed and opaque. A test does not recompute them.

## The round-trip key

The key in `keyrequest/poll-approved.json` is

    example-key-0123456789abcdefghijklmnopqrstuvw

Its SHA-256, lowercase hex, is

    097628f28c9143508f639c6a239bd2d6f4cc362062345c6cf7f8c1132cb12fd1

and that value is the `sha256` of the entry `alice@laptop` (role `agent`) in
`keylist/full.json`. A test can store the approved key, serve the list, and
check that the key is accepted.

    printf '%s' 'example-key-0123456789abcdefghijklmnopqrstuvw' | sha256sum

## Files

| File | Exchange |
|---|---|
| `keylist/full.json` | `200` list of four entries with an `ETag`. |
| `keylist/empty.json` | `200` `{"keys": []}`. |
| `keylist/not-modified.json` | `304`, no body, the `ETag`. |
| `keyrequest/request.json` | The body a tool files (no status). |
| `keyrequest/created.json` | `201`, every reply field. |
| `keyrequest/poll-pending.json` | `202 pending`. |
| `keyrequest/poll-slow-down.json` | `429 slow_down`, interval 5. |
| `keyrequest/poll-approved.json` | `200 approved` with the key. |
| `keyrequest/poll-denied.json` | `403 denied`. |
| `keyrequest/poll-expired.json` | `410 expired`. |
| `keyrequest/poll-consumed.json` | `410 consumed`. |
| `keyrequest/poll-unauthorized.json` | `401` error body. |
| `keyrequest/discovery.json` | `GET /v1/key-request` `200` with a `request_url`. |
| `keyrequest/discovery-none.json` | `GET /v1/key-request` `404`, no body. |
| `keyrequest/scenarios.json` | Not an Example Object. Named poll sequences; each step names a fixture of that directory, an optional `override` of members of its `value`, and the wait before the poll. |

## Which part of the spec references which file

`spec/key-request.yaml` references every file that has a body:
`keylist/full.json` and `keylist/empty.json` (200 of the list),
`keyrequest/request.json` (request body of the filing),
`keyrequest/created.json` (201), the six poll fixtures (`poll-approved` 200,
`poll-pending` 202, `poll-unauthorized` 401, `poll-denied` 403,
`poll-expired` and `poll-consumed` 410, `poll-slow-down` 429) and
`keyrequest/discovery.json` (200 of discovery). The files without a body
(`keylist/not-modified.json`, `keyrequest/discovery-none.json`) and
`scenarios.json` are named in the prose of the spec only; the tests in this
repository check them.
