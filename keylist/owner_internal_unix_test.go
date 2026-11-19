//go:build unix

package keylist

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/resurgentech/keyclient/keyclienttest"
)

// K-2: the cache's owner is compared with the effective uid, the identity the
// process acts as, not the real uid.
func TestK2_CacheOwnerComparedWithEffectiveUID(t *testing.T) {
	l := keyclienttest.NewList(t)
	l.FailWith(keyclienttest.Status(500, ""))
	s := newInternal(t, l, nil)
	sum := sha256.Sum256([]byte("key-from-cache"))
	body := fmt.Sprintf(`{"keys":[{"name":"alice@laptop","role":"agent","sha256":%q}]}`, hex.EncodeToString(sum[:]))
	if err := os.WriteFile(s.o.CacheFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Dir(s.o.CacheFile)); err != nil || !fi.IsDir() {
		t.Fatal(err)
	}
	me := os.Geteuid()
	geteuid = func() int { return me + 1 } // the process acts as another user
	t.Cleanup(func() { geteuid = os.Geteuid })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.Lookup("key-from-cache"); ok {
		t.Fatal("a cache owned by a uid other than the effective one was loaded")
	}
}
