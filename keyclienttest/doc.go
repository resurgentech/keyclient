// Package keyclienttest holds fakes for tests of tools and services that use the
// keyclient packages, in this module and in importing modules. They play the
// contract of docs/CONTRACT.md with the canonical fixtures (the testdata of this
// module, read through keyclient.Fixtures), so a test that passes against them
// speaks the contract.
//
// Both fakes run an httptest.Server that is closed in t.Cleanup, and both are
// safe for concurrent use: their state is guarded by a mutex, and every method
// may be called while requests are in flight. A missing fixture is a panic.
//
// # Provider
//
// [NewProvider] is a fake key provider that also plays the discovery endpoint
// of a service. A test of a tool (a user of package request) does this:
//
//   - Passes [Provider.URL] as the tool's ServiceURL, or [Provider.RequestURL]
//     as its override. [Provider.SetDiscovery] turns the discovery answer
//     between 200 and 404.
//   - Calls [Provider.SetInterval] with 1 (the default) so polling is quick:
//     the interval of the fixtures is 3 seconds and would make tests slow.
//   - Scripts what the next polls answer with [Provider.Script], built from
//     the [Outcome] constructors ([Pending], [SlowDown], [Approved], [Denied],
//     [Expired], [Consumed], [Unauthorized], [Status]) or from a named
//     [Scenario] of the fixtures. [Provider.FileWith] changes what filing
//     answers, for tests of 429 and 503.
//   - Afterwards asserts on what the tool did: [Provider.Filed] (the bodies it
//     filed), [Provider.Polls] (when it polled, and whether it presented the
//     right secret), and that [Provider.PollSecret] never appears in anything
//     the tool printed or wrote.
//
// # List
//
// [NewList] is a fake key-list endpoint. A test of a service (a user of package
// keylist) points the source's URL at [List.URL], then changes the list with
// [List.Set], [List.Add], [List.Remove] or [List.LoadFixture], breaks the
// endpoint with [List.FailWith] and [List.Recover], requires a pull token with
// [List.RequireToken], holds pulls with [List.Block], and asserts on
// [List.Pulls] and [List.Requests]: how many pulls arrived, and with which
// If-None-Match and Authorization.
package keyclienttest
