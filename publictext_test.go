package keyclient_test

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// privateWordHashes holds the SHA-256 (lowercase hex) of words that must never
// appear in this public repository, in any case: the names of one particular
// deployment, its hosts, products and people. The list holds hashes so that it
// does not publish the words it guards. It includes the deployment's lab name
// and the prefix of its host names, dot included.
var privateWordHashes = map[string]bool{
	"1837150b063c726b8a8dc5a78837e095243baffee3576f3dfa241b9a67bda8d5": true,
	"918e1977b12c29ff99cc497834cd8a4167246abe8a8f6a058ac758f3d7d8a01f": true,
	"1066f933058642cb2e87d684bc6712bc8c221c322b418124d864a2d9306e6771": true,
	"98ce04cd7742fe60a0ed09e4cbc391fee4e57ec9f6ed8b23de3221e6ebf50cb4": true,
	"f843198bc1db12c83bd5e98f717035d4c5dea92335121b7214a1218868a9010f": true,
	"27d300fe53b3b94f115cfd63be02d868bcb8f755e56893709418084c1bfab1cd": true,
	"4e1892ae621d36150cf0fe877428b79efb35dff59fb8a2e53d128cf139e5081e": true,
	"e1493bafb22b0a896babc96e498b6d28f7e583311d9dec7196c8b86b82853d9d": true,
	"913f8566936acaa4532730a7024fd5a3355b889daf86231d0dbf0dfadb9e085b": true,
	"b175fe00f9bc863cb129b985dbfee4bf529345f3fe0dff48564b9c6f377e9db2": true,
	"c7426ddc70261112ff352b6b478018a3e5336cfda46097cbaaa8b112f0c9c0d2": true,
	"f7d182e6d1936b41527b699a8b089cd322a8329be204b5c1eb8551466f702675": true,
	"c5e12842fb86ea0bca6c2054dd9bce6401a6cbfe664d35e23245b38d2a865852": true,
	"07a80d4ee0cb58016eae69276cd6d00c98c5dd1c95ab6bbbd7e0567aa28444ba": true,
	"d6e0ff2b7a534a9750d13545971476557164f72cb92ad74be7f145491ec4acf4": true,
}

// privateWordLengths are the lengths of the guarded words.
var privateWordLengths = []int{4, 5, 6, 7, 8, 11}

// Local, gitignored paths that are not part of the published tree.
var skipDirs = map[string]bool{".git": true, "tmp": true, "docs/reports": true}
var skipFiles = map[string]bool{"ROADMAP.md": true}

func privateHit(text string) (string, bool) {
	text = strings.ToLower(text)
	for _, n := range privateWordLengths {
		for i := 0; i+n <= len(text); i++ {
			sum := sha256.Sum256([]byte(text[i : i+n]))
			if privateWordHashes[hex.EncodeToString(sum[:])] {
				return text[i : i+n], true
			}
		}
	}
	return "", false
}

func TestPrivateHitFindsGuardedWords(t *testing.T) {
	// The words are assembled so this file itself stays clean.
	for _, w := range []string{"lab" + "two", "INFRA" + ".", "see " + "infra" + ".example for"} {
		if _, ok := privateHit(w); !ok {
			t.Errorf("privateHit(%q) found nothing", w)
		}
	}
	for _, w := range []string{"infrastructure", "provider.example", "alice@laptop", "laboratory"} {
		if hit, ok := privateHit(w); ok {
			t.Errorf("privateHit(%q) = %q, want no hit", w, hit)
		}
	}
}

// TestTreeNamesNoPrivateWords reads every file of the published tree, the
// test files included, and fails on any guarded word.
func TestTreeNamesNoPrivateWords(t *testing.T) {
	files := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		slash := filepath.ToSlash(path)
		if d.IsDir() {
			if skipDirs[slash] {
				return filepath.SkipDir
			}
			return nil
		}
		if skipFiles[slash] {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		for n, line := range strings.Split(string(b), "\n") {
			if _, ok := privateHit(line); ok {
				// The word itself is not printed: the log may be public too.
				t.Errorf("%s:%d: names a private word", slash, n+1)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 10 {
		t.Fatalf("walked only %d files; is the test running at the module root?", files)
	}
}
