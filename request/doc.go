// Package request is the tool side of the key-request contract (docs/CONTRACT.md,
// "Key request"): a tool that finds no token asks a key provider for one, a
// person approves the request in a browser, and the tool stores the key it is
// given where it reads its token.
//
// A tool calls [Ensure] when it needs its token, with the file it reads the
// token from. If that file holds a token, Ensure returns it and does nothing
// else: no network, no request. Only when the file holds none, and a request
// URL is known (from [Options.Override] or from discovery at
// [Options.ServiceURL]), does Ensure file a request, call [Options.Notify] once
// with a [Notice] (the one message for the user: approve at a URL and check the
// code), and poll until the person decides. On approval it returns a [Result]:
// the key, and the name and role the provider granted.
//
// Two ways to wait, chosen by [Options.Wait]:
//
//   - Wait true (a command line tool in a terminal; see [IsTerminal]): Ensure
//     polls until the request settles and returns the Result or an error.
//   - Wait false (a server, or a tool whose output is not a terminal): Ensure
//     returns an [*ErrPending] once the request is filed or resumed, and polls
//     in a background goroutine bounded by the context. The caller reports the
//     notice, and the next call of Ensure finds the token (or, while the poll
//     runs in this process, gets the same *ErrPending). Pass a context that
//     outlives the call: the context of one incoming request ends too soon.
//
// # Trust and transport
//
// The tool trusts the discovery answer exactly as much as it trusts the
// service. Whoever can answer for the service can send the tool to a request
// URL of their choosing, relay the tool's request to the real provider, and
// receive the key while the person sees the genuine approval page and a
// matching code. Reaching the service over https is what protects the flow. So:
//
//   - Plain http is refused for every URL of the flow (ServiceURL, Override,
//     the request_url from discovery, poll_url, approve_url), before anything
//     is sent to it, unless its host is a loopback address ("localhost",
//     127.0.0.0/8, ::1) or [Options.AllowInsecureHTTP] is set.
//   - A flow never downgrades: once one step is https, every later URL must be
//     https, whatever AllowInsecureHTTP says.
//   - Redirects are never followed.
//
// What the provider sends is checked before anything is shown, stored or
// polled: a 201 whose request_id, user_code or approve_url breaks the contract's
// rules, or holds the poll secret, is refused. Text from the provider that is
// shown (a [ProviderError]'s Code and Message, the message of a 401) is reduced
// to printable characters first, and the poll secret is then redacted from it.
//
// # One flow per token
//
// Ensure takes an exclusive lock (flock) on TokenPath + ".lock" before it reads
// the token and pending files, and holds it until the flow ends, so two
// processes never file two requests or poll one request at once: the second
// waits (bounded by its context), then finds the token the first stored. With
// Wait false the lock is released when Ensure returns the *ErrPending; the
// background poll then goes on under this process's own bookkeeping only, so
// another process that starts meanwhile may resume and poll the same request.
// Within one process, callers for one TokenPath join the running flow: no
// second filing and no second notice. Key requests need the lock: on platforms
// other than Linux, macOS and the BSDs, Ensure returns a token already on disk
// but reports that key requests are not supported instead of filing or
// resuming one.
//
// What lands on disk:
//
//   - TokenPath: the key, mode 0600, written atomically. Its directory is
//     created with mode 0700 when it does not exist.
//   - TokenPath + ".lock": an empty file, mode 0600, the lock. It is left in
//     place.
//   - TokenPath + ".pending" ([PendingPath]): mode 0600, while a request is open.
//     It holds the request id, poll URL, poll secret, approve URL, user code,
//     role, tool, interval, expiry, and the ServiceURL, Override and request URL
//     it was filed with, so that a later run resumes the request instead of
//     filing a second one. A later run with another ServiceURL or Override
//     drops it and files anew. Its values are checked again when it is resumed;
//     a file that is not a regular file of mode 0600, or whose values break the
//     rules, is refused. It is removed once the request settles (key stored,
//     denied, expired, consumed) or expires, and only while it still holds that
//     request. It is kept when the tool stops without a verdict (context ended,
//     401, an unexpected status).
//
// Limits: each HTTP call is bounded by [RequestTimeout], and at most
// [MaxReply] bytes of a reply are read. The request is treated as expired at
// most 24 hours after it was filed, whatever expires_in says. When the next
// poll would fall after the expiry, the tool polls once more at the expiry.
//
// The poll secret and the key are written to the pending and token files and
// nowhere else: no error, [Notice] or [ErrPending] text ever contains either.
//
// The package uses the standard library only.
package request
