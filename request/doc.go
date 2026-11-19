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
//     A tool that prints the notice and exits still collects its key: the
//     pending file records when the next poll is due, and a later call makes
//     that poll before it returns once it is due, returning the key if the
//     request was approved. A call with Wait false never waits for a person
//     or for another process; only the time limits of its network calls
//     bound it.
//
// Notice.ApproveURL comes from the provider. Never pass it (or
// Notice.String()) to a shell or a URL opener without the user's consent.
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
// rules, or holds the poll secret (as it is, or URL-escaped), is refused. Text from the provider that is
// shown (a [ProviderError]'s Code and Message, the message of a 401) is reduced
// to printable characters first, and the poll secret is then redacted from it.
//
// # One flow per token
//
// The states and transitions this package follows are written out, row by
// row, in docs/STATES.md (machines 2 and 3); the tests are named after those
// rows.
//
// A token on disk is returned without the lock, and nothing else is touched.
// Otherwise the order is:
//
//  1. When the request URL must come from discovery (no Override) and there is
//     no pending file, discovery runs first, without the lock: a service
//     without a provider ([ErrNoProvider]) leaves nothing on disk, not even
//     the directory.
//  2. Ensure takes an exclusive lock (flock) on TokenPath + ".lock", making the
//     file (and its directory) when missing, and holds it until the flow ends:
//     with Wait false, until the background poll ends. So two processes never
//     file two requests or poll one request at once.
//  3. Under the lock it removes the temporary files an interrupted write of
//     the token or pending file left, reads the token file again (a token
//     another holder stored is returned, and a pending file left beside it by
//     a crash is removed), then resumes the request in the pending file or
//     files a new one. A pending file that appeared since step 1 is resumed
//     and the discovered URL is not used; one of another ServiceURL or
//     Override is removed and the discovered URL is used.
//
// When another process holds the lock: with Wait true, Ensure shows the notice
// of the request in the pending file (marked resumed) once, waits for the lock
// (bounded by its context), then finds the token or resumes that request
// without showing it again. With Wait false it returns at once: an
// *ErrPending built from the pending file, whose background goroutine waits
// for the lock and then does the same; or [ErrBusy] when the pending file
// holds no current request.
//
// Within one process, callers for one TokenPath join the running flow: no
// second filing and no second notice. Key requests need the lock: on platforms
// other than Linux, macOS and the BSDs, Ensure returns a token already on disk
// but returns an error that wraps errors.ErrUnsupported instead of filing or
// resuming one. A file system without flock fails closed the same way: the
// error names the lock file, and nothing is filed.
//
// What lands on disk:
//
//   - TokenPath: the key, mode 0600, written atomically. Its directory is
//     created with mode 0700 when it does not exist.
//   - TokenPath + ".lock": an empty file, mode 0600, the lock. It is left in
//     place. A lock file that is a link, not a regular file, not owned by the
//     user, or writable by group or others is refused with an error that
//     names it.
//   - TokenPath + ".pending" ([PendingPath]): mode 0600, while a request is open.
//     It holds the request id, poll URL, poll secret, approve URL, user code,
//     role, tool, the current interval and when the next poll is due
//     (rewritten after each poll that keeps the request open), the expiry, and
//     the ServiceURL, Override and request URL it was filed with, so that a
//     later run resumes the request, at the same pace, instead of filing a
//     second one. A later run with another ServiceURL or Override drops it and
//     files anew. Its values are checked again when it is resumed; a file that
//     is not a regular file of mode 0600 owned by the user, or whose values
//     break the rules, is refused. A next poll time in the past (or none, in a
//     file of an older version) means now; one further away than the interval
//     means one interval from now. It is removed once the request settles (key
//     stored, denied, expired, consumed) or expires, and only while it still
//     holds that request. It is kept when the tool stops without a verdict
//     (context ended, 401, an unexpected status). Only the lock holder writes
//     or removes it.
//
// Limits: each HTTP call is bounded by [RequestTimeout], and at most
// [MaxReply] bytes of a reply are read. The request is treated as expired at
// most 24 hours after it was filed (or, when resumed, from now), whatever
// expires_in says. When the next poll would fall after the expiry, the tool
// polls once more at the expiry.
//
// The poll secret and the key are written to the pending and token files and
// nowhere else: no error, [Notice] or [ErrPending] text ever contains either.
//
// The package uses the standard library only.
package request
