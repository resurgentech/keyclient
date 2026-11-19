# Contract

Two contracts between a **key provider**, the **services** that check keys, and the **tools** that need one. This
file is the canonical text. The OpenAPI document is [`spec/key-request.yaml`](../spec/key-request.yaml) and the
canonical fixtures are in [`testdata/`](../testdata/README.md). The Go packages in this module are the reference
consumers: `keylist` for a service, `request` for a tool. A change is made here first, then in the spec, the
fixtures and the code.

## Key list

A service can accept personal keys in addition to the tokens it already has. A **key provider** publishes, for the
service, a list of `{name, role, sha256}`. The service pulls it on a timer and checks each presented key against
it. The provider never sends a key, only the SHA-256 of one.

Terms used here and in the next section. A **service** checks keys on its API. A **provider** is the HTTP server
that serves the list and takes key requests. A **tool** is a client of the service that needs a key. A **consumer**
is the service's side of the list: the code that pulls it. A key is an opaque secret string.

Settings (the names the reference consumers use). Each is off unless `personal_keys.url` is set:

| Key | Rule |
|---|---|
| `personal_keys.url` | The list URL. If absent, nothing in this section happens: no request, no cache file, no timer. |
| `personal_keys.token_file` | Optional. File holding the pull token. Surrounding whitespace is trimmed. |
| `personal_keys.cache_file` | File where the last good list is kept. |
| `personal_keys.refresh` | Time between pulls. Default `1m`. Must be greater than zero. |
| `personal_keys.refresh_on_miss` | Least time between two pulls triggered by an unknown key. Default `5s`; `0` turns refresh on miss off. See [When a key starts and stops working](#when-a-key-starts-and-stops-working). |

### Request

`GET <url>` with:

- `Accept: application/json`.
- `Authorization: Bearer <pull token>` when `token_file` is set.
- `If-None-Match: <ETag>` when the consumer holds a list it received in this process, with the `ETag` that came
  with it. The first request after a start is unconditional.

The consumer gives each request a timeout (10 seconds is suggested), and reads at most 4 MiB of body.

### Responses

| Status | Meaning | Consumer action |
|---|---|---|
| `200` | The current list. Body `{"keys":[{"name","role","sha256"}, ...]}`, content type `application/json`, with an `ETag` header holding a quoted, strong validator that the consumer treats as opaque. | Validate, apply, cache, remember the `ETag`. |
| `304` | Unchanged since `If-None-Match`. No body. | Do nothing. |
| `401` | The pull token is missing or wrong. Body `{"error": "<code>", "message": "..."}`. | A failure (below). Log it as an error; this needs an operator. |
| `404` | The provider has no list for this URL. Same error body. | A failure. It is **not** an empty list and revokes nothing. |
| anything else, a bad body, a network error, a timeout | | A failure. |

A **failure** keeps the last good list, changes nothing else, and is retried at the next refresh. The consumer logs
the first failure and the recovery, not every tick.

An empty list, `{"keys": []}`, is a valid `200`. It revokes every pulled key at the next apply.

### List and entry rules

- The body is a JSON object with one required member, `keys`, an array. Unknown members of the object or of an
  entry are ignored, so a later version may add them. The provider sends none in this version.
- Each entry has `name`, `role` and `sha256`, all strings:
  - `name` matches `^[A-Za-z0-9_.@-]{1,64}$`. It identifies the key's owner and machine, for example
    `alice@laptop`.
  - `role` matches `^[A-Za-z0-9_.-]{1,32}$`. The consumer decides which roles it knows.
  - `sha256` is the lowercase hexadecimal SHA-256 of the key's bytes: `^[0-9a-f]{64}$`.
- A body that is not valid JSON of this shape, or holds an entry that breaks these patterns, is a bad body: a
  failure. The consumer cannot tell what was meant, so it keeps the last good list.
- The provider sorts entries by `name` (byte order) and names are unique in a list, so one set of keys always
  gives one body and one `ETag`. The consumer does not rely on order.

### What the consumer does with the list

When it applies a good `200` (and when it loads the cache), each entry is accepted or skipped:

- **Unknown role.** An entry whose role the consumer does not know is skipped and logged.
- **Local tokens win.** An entry whose `name` or `sha256` is already in the consumer's own token source (the
  tokens the service's operator configures directly) is skipped and logged. A pulled list can therefore never
  replace or shadow a local token.
- **Repeats.** If a `name` or a `sha256` appears twice in one list, the first is kept and the rest are skipped and
  logged.
- The log line gives the name and the reason. **It never gives a hash**, a pull token or a presented key.

The accepted entries replace the live list in one atomic swap: a request sees the whole old list or the whole new
one. A key absent from the new list is refused from the swap on.

To check a presented key, the consumer computes its SHA-256 and compares it, in constant time, with every entry's
`sha256`, with no early exit. A match yields the entry's `name` and `role`.

### Cache

- After each good `200`, the body is written to `cache_file` atomically (a temporary file in the same directory,
  then a rename), with mode 0600. A failed cache write is logged; the list is still applied.
- At start the consumer loads `cache_file` first, with the same checks, then pulls at once. A missing cache is an
  empty list. A cache that does not parse is ignored and logged.
- So a restart with the provider down keeps working with the last good list. The service starts even when the
  provider is unreachable. The `ETag` is not stored in the cache.

### When a key starts and stops working

A key is valid at a service from the first pull that lists it, and invalid from the first pull that does not.

- **Provider rule.** A key is in its service's list from the moment the provider delivers it.
- **Refresh on miss.** A new key works at once:
  - **Trigger.** A bearer token that matches neither the consumer's own token source nor the pulled list triggers
    one immediate conditional pull, sent with `If-None-Match`, and the token is checked again.
  - **Throttle.** At most one miss-triggered pull starts per `personal_keys.refresh_on_miss` (default `5s`; `0`
    turns the feature off). The throttle covers all callers together.
  - **Waiting.** Misses that arrive while that pull is in flight, the one that started it among them, wait for it,
    up to 2 s. Every other miss inside the window gets `401` at once, with no pull.
  - The miss-triggered pull never moves the timer: the next timed pull comes when it would have come anyway.
- **Revocation** is unchanged: a revoked key stops within the normal `refresh`, or sooner if a miss-triggered pull
  lands first.

A tool that has just received a key can therefore use it at once and needs no retry.

### Transport

The list holds no key, but whoever can alter the response can add an entry and so grant access. Use `https`, or a
network path you trust, for `personal_keys.url`.

## Key request

A tool that has no key can ask the provider for one. A person approves the request in a browser. The tool polls
until it is told the outcome, and on approval it receives its key once and stores it where it reads its token.
This uses the terms of the key list section. The key it receives is checked by the service through the key list.

```
Tool                       Service                  Provider                  Person
 | GET /v1/key-request        |                        |                        |
 |--------------------------->|                        |                        |
 | 200 {request_url}          |                        |                        |
 |<---------------------------|                        |                        |
 | POST request_url {name_hint, host, tool, role}      |                        |
 |---------------------------------------------------->|                        |
 | 201 {request_id, poll_url, poll_secret,             |                        |
 |      approve_url, user_code, interval, expires_in}  |                        |
 |<----------------------------------------------------|                        |
 | tells the user: approve at approve_url, code user_code                       |
 |---------------------------------------------------------------------------->|
 |                                                     |  opens approve_url,    |
 |                                                     |  checks the code,      |
 |                                                     |  approves              |
 |                                                     |<-----------------------|
 | POST poll_url, Bearer poll_secret (after interval)  |                        |
 |---------------------------------------------------->|                        |
 | 202 pending, or 429 slow_down                       |                        |
 |<----------------------------------------------------|                        |
 | POST poll_url ...                                   |                        |
 | 200 approved {name, role, key}   (once)             |                        |
 |<----------------------------------------------------|                        |
 | writes the key atomically, mode 0600                |                        |
 | later: calls the service with the key; the service pulls its list            |
```

### Discovery

The service advertises where its keys are requested.

- `GET /v1/key-request`. No authentication.
- `200` with `{"request_url": "<absolute URL>"}` when the service is configured with `personal_keys.request_url`.
- `404` when it is not. The tool must not rely on the body of a `404`.

A tool may be given a request URL directly (a flag or an environment variable). That value replaces discovery,
and discovery is then not called. A tool with a request URL from neither source behaves as it did before this
contract: a missing key is the same error as today. Discovery that fails any other way (a network error, a status
other than `200` and `404`, a `request_url` that is not an absolute `http` or `https` URL) is an error the tool
reports; it files nothing.

### Filing a request

`POST <request_url>` with `Content-Type: application/json` and a body of at most 2048 bytes, for example:

```json
{"name_hint": "alice@laptop", "host": "laptop", "tool": "mytool/0.3.0", "role": "agent"}
```

All four fields are required. An unknown field is rejected.

| Field | Rule | Meaning |
|---|---|---|
| `name_hint` | `^[A-Za-z0-9_.@-]{1,64}$` | A suggested name, shown to the approver for information. |
| `host` | `^[A-Za-z0-9_.-]{1,64}$` | The machine the tool runs on. |
| `tool` | `^[A-Za-z0-9_.@/+-]{1,64}$` | The tool and its version. |
| `role` | `^[A-Za-z0-9_.-]{1,32}$` | The role the tool wants. Each tool documents a default. The approver may grant another. |

Reply `201` with content type `application/json`:

| Field | Type and rule |
|---|---|
| `request_id` | String. Opaque. |
| `poll_url` | String. Absolute URL. |
| `poll_secret` | String. Opaque. A secret: only the requester holds it. |
| `approve_url` | String. Absolute URL where a person approves. It never carries `poll_secret`. |
| `user_code` | String for display, such as `BCDF-GHJK`. |
| `interval` | Integer, at least 1. Seconds between polls. |
| `expires_in` | Integer, at least 1. Seconds until the request expires. |

Errors from filing carry `{"error": "<code>", "message": "..."}` (content type `application/json`):

| Status | Meaning | Tool action |
|---|---|---|
| `400` | A field is missing, unknown or breaks its rule, or the role is not offered. | Report. Do not retry unchanged. |
| `401` | The provider will not take requests from this client. | Report. |
| `404` | The provider has no such service. | Report. |
| `413` | The body is over 2048 bytes. | Report. |
| `429` | Too many requests from this source. `Retry-After` gives seconds. | Report, with the wait. |
| `503` | The provider is at capacity. | Report. May retry later. |
| other `5xx`, network error | | Report. May retry later. |

The codes the reference provider uses are `bad_request`, `unknown_role`, `unauthorized`, `not_found`, `too_large`,
`rate_limited`, `capacity` and `internal`. A tool shows `message` to the user and does not branch on `error`.

The responses to filing and to polling carry `Cache-Control: no-store`.

### What the tool tells the user

One message: **approve at `approve_url` and check that the code `user_code` matches** the one the approver sees.
It states the role and tool it asked for. It never shows `poll_url`, `poll_secret` or a key. In a terminal the
message goes to standard error. In a protocol where a tool returns results, it goes in the result.

### Polling

`POST <poll_url>` with `Authorization: Bearer <poll_secret>`. There is no request body; a body is ignored.

The first poll comes no sooner than `interval` seconds after the `201` arrived. Each later poll comes no sooner
than the current interval after the previous poll's response arrived. The current interval is the `interval` of
the latest response that carried one.

| Status | Body | Meaning | Tool action |
|---|---|---|---|
| `202` | `{"status":"pending","interval":N}` | No decision yet. | Poll again after `N` seconds. |
| `429` | `{"status":"slow_down","interval":N}` | The tool polled too soon. | Use `N`, which is the old interval plus 2. |
| `200` | `{"status":"approved","name":"...","role":"...","key":"..."}` | Approved. Sent exactly once. | Store the key. Stop. |
| `403` | `{"status":"denied"}` | A person denied it. Final. | Stop and report. |
| `410` | `{"status":"expired"}` or `{"status":"consumed"}` | Final. `expired`: the time ran out. `consumed`: the key was already handed out. | Stop and report. |
| `401` | `{"error":"...","message":"..."}` | The poll secret is wrong, or the id is unknown. The two cannot be told apart. A `401` never changes the request. | Report. Keep the request; it may be retried. |
| `5xx`, network error, timeout | | Transient. | Keep polling at the current interval until expiry. |
| any other status | | | Report and stop polling now. Keep the request. |

**`slow_down` arithmetic.** The provider counts a poll as too soon when fewer than `interval` seconds have passed
since the previous poll was handled. It answers `429` with the old interval plus 2, and the new value is the
request's interval from then on, in every later reply. A too-soon poll counts as a poll for timing. The interval
never goes down. The tool uses the new value for its next wait. A `429` with a body the tool cannot read is treated
as `slow_down` with the interval plus 2.

**The approved reply.** `key` is an opaque string of 16 to 512 printable ASCII characters with no whitespace.
`name` is the name the key has in the provider's list. `role` is the role granted, which may differ from the role
asked. The provider generates the key when it answers this poll and keeps only its hash. If this reply is lost,
it cannot be sent again: the next poll gets `410 consumed`, and the tool must file a new request.

### Storing the key

On `200`, the tool writes the key atomically (a temporary file in the same directory, then a rename) to the file
it reads its token from, with mode 0600. It creates the directory with mode 0700 when it does not exist. It then
carries on. No other file changes. A reply with a key that breaks the rule above is an error; nothing is written.

### Stopping

The tool stops when it gets `403` or `410`, or `expires_in` seconds after the `201` arrived, whichever is first.
It then reports the outcome and, if the user still needs a key, files a new request only on the next use.

### Resuming a pending request

A filed request stays valid until it expires, even if the tool exits. A tool may keep `request_id`, `poll_url`,
`poll_secret` and the expiry time in a file readable only by its user (mode 0600), and on its next run poll that
request instead of filing a new one. A tool must not file a second request while it holds an unexpired one,
because the provider limits how many requests one source may have open. It removes the stored request when the
request settles (a final status, or the key stored) or expires.

### Defaults unchanged

A tool that already has a token never makes a request. With no request URL (from discovery or an override), a
missing token is the same error as before.
