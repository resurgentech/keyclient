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
// code), and poll until the person decides.
//
// Two ways to wait, chosen by [Options.Wait]:
//
//   - Wait true (a command line tool in a terminal; see [IsTerminal]): Ensure
//     polls until the request settles and returns the token or an error.
//   - Wait false (a server, or a tool whose output is not a terminal): Ensure
//     returns an [*ErrPending] at once, and polls in a background goroutine
//     bounded by the context. The caller reports the notice, and the next call
//     of Ensure finds the token (or, while the poll runs in this process, gets
//     the same *ErrPending). Pass a context that outlives the call: the context
//     of one incoming request ends too soon.
//
// What lands on disk:
//
//   - TokenPath: the key, mode 0600, written atomically. Its directory is
//     created with mode 0700 when it does not exist.
//   - TokenPath + ".pending" ([PendingPath]): mode 0600, while a request is open.
//     It holds the request id, poll URL, poll secret, approve URL, user code,
//     role, tool, interval and expiry, so that a later run resumes the request
//     instead of filing a second one. It is removed once the request settles (key
//     stored, denied, expired, consumed) or expires. It is kept when the tool
//     stops without a verdict (context ended, 401, an unexpected status).
//
// The poll secret and the key are written to those two files and nowhere else:
// no error, [Notice] or [ErrPending] text ever contains either.
//
// The package uses the standard library only.
package request
