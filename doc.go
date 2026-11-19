// Package keyclient is the client side of two small HTTP contracts for personal
// keys, stated in docs/CONTRACT.md of this module:
//
//   - A tool that has no key asks a key provider for one, a person approves the
//     request in a browser, and the tool stores the key it is given. That is
//     package [github.com/resurgentech/keyclient/request]: a tool calls
//     [github.com/resurgentech/keyclient/request.Ensure] when it finds no token.
//   - A service pulls from the provider the list of the SHA-256 hashes of the
//     keys it should accept, and checks presented keys against it. That is
//     package [github.com/resurgentech/keyclient/keylist]: a daemon builds a
//     [github.com/resurgentech/keyclient/keylist.Source], starts it, and
//     calls [github.com/resurgentech/keyclient/keylist.Source.LookupOrRefresh]
//     after its own tokens have missed.
//
// Package [github.com/resurgentech/keyclient/keyclienttest] holds fakes of a
// provider and of a key list for tests. This package itself only exposes the
// contract's canonical fixtures.
//
// The module uses the standard library only.
package keyclient

import (
	"embed"
	"io/fs"
)

//go:embed testdata/keylist/*.json testdata/keyrequest/*.json
var testdata embed.FS

// Fixtures returns the contract's canonical fixtures (the testdata directory of
// this module, see its README.md), rooted so that paths read
// "keylist/full.json" and "keyrequest/created.json". A provider or a consumer
// in another module can test against them.
func Fixtures() fs.FS {
	sub, err := fs.Sub(testdata, "testdata")
	if err != nil {
		panic(err) // the embedded tree always has testdata/
	}
	return sub
}
