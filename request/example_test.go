package request_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"

	"github.com/resurgentech/keyclient/request"
)

// exampleProvider is a tiny stand-in for a service and its key provider: it
// serves discovery, a 201 (interval 1) and an approved poll with a fixed key.
// The tests use keyclienttest instead; examples cannot, it needs testing.TB.
func exampleProvider() *httptest.Server {
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("GET /v1/key-request", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"request_url":%q}`, srv.URL+"/requests")
	})
	mux.HandleFunc("POST /requests", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"request_id":"req-1","poll_url":%q,"poll_secret":"example-poll-secret-0123456789","approve_url":%q,"user_code":"BCDF-GHJK","interval":1,"expires_in":600}`,
			srv.URL+"/poll", srv.URL+"/approve")
	})
	mux.HandleFunc("POST /poll", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"approved","name":"alice@laptop","role":"agent","key":"example-key-0123456789abcdefghijklmnopqrstuvw"}`)
	})
	srv = httptest.NewServer(mux)
	return srv
}

// A command line tool that finds no token file asks for a key. In a terminal
// it waits for the approval (Wait: request.IsTerminal(os.Stderr) in real code);
// the Notify callback tells the user where to approve, on standard error.
func ExampleEnsure() {
	srv := exampleProvider()
	defer srv.Close()
	dir, _ := os.MkdirTemp("", "keyclient-example-")
	defer os.RemoveAll(dir)

	res, err := request.Ensure(context.Background(), request.Options{
		ServiceURL: srv.URL,                     // the service's base URL (loopback http here; use https)
		TokenPath:  filepath.Join(dir, "token"), // where the tool reads its token
		Role:       "agent",                     // the tool documents its default
		NameHint:   "alice@laptop",              // user@host
		Tool:       "mytool/0.3.0",              // name and version
		Wait:       true,                        // request.IsTerminal(os.Stderr) in real code
		Notify: func(n request.Notice) {
			// A real tool prints n.String() to standard error.
			fmt.Println("approve code:", n.UserCode, "role:", n.Role, "tool:", n.Tool)
		},
	})
	if errors.Is(err, request.ErrNoProvider) {
		fmt.Println("no key provider: keep the usual 'no token' error")
		return
	}
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("stored, length", len(res.Token), "name:", res.Name, "role:", res.Role)
	// Output:
	// approve code: BCDF-GHJK role: agent tool: mytool/0.3.0
	// stored, length 45 name: alice@laptop role: agent
}

// A long-running server (an MCP-style tool) must not block: with Wait false
// Ensure returns an *ErrPending at once. The server reports the notice in its
// result, and the next call finds the token. The context must outlive the call.
func ExampleEnsure_background() {
	srv := exampleProvider()
	defer srv.Close()
	dir, _ := os.MkdirTemp("", "keyclient-example-")
	defer os.RemoveAll(dir)

	opts := request.Options{
		ServiceURL: srv.URL,
		TokenPath:  filepath.Join(dir, "token"),
		Role:       "agent",
		NameHint:   "alice@laptop",
		Tool:       "mytool/0.3.0",
		Wait:       false,
	}
	_, err := request.Ensure(context.Background(), opts)
	var pending *request.ErrPending
	if !errors.As(err, &pending) {
		fmt.Println("unexpected:", err)
		return
	}
	// Report pending.Notice (or pending.Error()) to the user in the tool's result.
	fmt.Println("pending, code", pending.Notice.UserCode, "ready:", pending.Ready())

	<-pending.Done() // a server does not wait; it checks later
	fmt.Println("ready:", pending.Ready(), "err:", pending.Err())

	// Now returns the stored token. It was on disk, so Name and Role are empty.
	res, err := request.Ensure(context.Background(), opts)
	fmt.Printf("token length %d name %q err: %v\n", len(res.Token), res.Name, err)
	// Output:
	// pending, code BCDF-GHJK ready: false
	// ready: true err: <nil>
	// token length 45 name "" err: <nil>
}
