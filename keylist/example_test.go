package keylist_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"

	"github.com/resurgentech/keyclient/keylist"
)

// listServer is a minimal provider: it serves a list of one key, to a puller
// that presents the pull token.
func listServer(pullToken, name, role, key string) *httptest.Server {
	body := fmt.Sprintf(`{"keys":[{"name":%q,"role":%q,"sha256":"%x"}]}`, name, role, sha256.Sum256([]byte(key)))
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+pullToken {
			http.Error(w, `{"error":"unauthorized","message":"no"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"v1"`)
		fmt.Fprint(w, body)
	}))
}

// A daemon builds a Source from its configuration, starts it, and in its
// authentication checks its own tokens first, then the pulled list; a miss is
// a 401.
func Example() {
	srv := listServer("pull-secret", "alice@laptop", "agent", "alice-key")
	defer srv.Close()
	dir, _ := os.MkdirTemp("", "keylist-example")
	defer os.RemoveAll(dir)
	tokenFile := filepath.Join(dir, "pull-token")
	os.WriteFile(tokenFile, []byte("pull-secret\n"), 0o600)

	// The daemon's own tokens: token to name. Its token names and hashes win
	// over the pulled list.
	own := map[string]string{"daemon-secret": "operator"}
	ownNames := map[string]bool{"operator": true}
	ownHashes := map[string]bool{}
	for tok := range own {
		ownHashes[fmt.Sprintf("%x", sha256.Sum256([]byte(tok)))] = true
	}

	src, err := keylist.New(keylist.Options{
		// A plain http URL is allowed here only because the host is loopback;
		// otherwise use https, or set AllowInsecureHTTP for a path you trust.
		URL:           srv.URL,
		TokenFile:     tokenFile,
		CacheFile:     filepath.Join(dir, "cache", "personal-keys.json"),
		Refresh:       keylist.DefaultRefresh,
		RefreshOnMiss: keylist.DefaultRefreshOnMiss,
		// Unknown roles and clashes with the daemon's own tokens are refused.
		Accept: keylist.AcceptRoles([]string{"agent", "full"}, func(e keylist.Entry) bool {
			return ownNames[e.Name] || ownHashes[e.SHA256]
		}),
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := src.Start(ctx); err != nil {
		fmt.Println(err)
		return
	}

	// The daemon's authentication middleware, as a function.
	authorize := func(r *http.Request) int {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if name, ok := own[tok]; ok {
			fmt.Println("own token:", name)
			return http.StatusOK
		}
		if name, role, ok := src.LookupOrRefresh(r.Context(), tok); ok {
			fmt.Println("personal key:", name, role)
			return http.StatusOK
		}
		return http.StatusUnauthorized
	}
	for _, tok := range []string{"daemon-secret", "alice-key", "stranger-key"} {
		r := httptest.NewRequest("GET", "/v1/things", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		fmt.Println(authorize(r))
	}
	// Output:
	// own token: operator
	// 200
	// personal key: alice@laptop agent
	// 200
	// 401
}
