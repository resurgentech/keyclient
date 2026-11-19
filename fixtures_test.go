package keyclient_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/resurgentech/keyclient"
)

type example struct {
	Summary     string            `json:"summary"`
	Description string            `json:"description"`
	Status      int               `json:"x-status"`
	Headers     map[string]string `json:"x-headers"`
	Value       json.RawMessage   `json:"value"`
}

func readExample(t *testing.T, fsys fs.FS, name string) example {
	t.Helper()
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		t.Fatal(err)
	}
	var e example
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return e
}

// exampleFiles is every fixture that is an OpenAPI Example Object.
var exampleFiles = []string{
	"keylist/full.json", "keylist/empty.json", "keylist/not-modified.json",
	"keyrequest/request.json", "keyrequest/created.json",
	"keyrequest/poll-pending.json", "keyrequest/poll-slow-down.json",
	"keyrequest/poll-approved.json", "keyrequest/poll-denied.json",
	"keyrequest/poll-expired.json", "keyrequest/poll-consumed.json",
	"keyrequest/poll-unauthorized.json",
	"keyrequest/discovery.json", "keyrequest/discovery-none.json",
}

func TestFixturesEmbedsEveryFile(t *testing.T) {
	fsys := keyclient.Fixtures()
	var got []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".json") {
			got = append(got, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]string{}, exampleFiles...), "keyrequest/scenarios.json")
	if len(got) != len(want) {
		t.Fatalf("Fixtures() has %d json files %v, want %d", len(got), got, len(want))
	}
	for _, name := range want {
		disk, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		emb, err := fs.ReadFile(fsys, name)
		if err != nil {
			t.Fatalf("Fixtures() lacks %s: %v", name, err)
		}
		if string(disk) != string(emb) {
			t.Errorf("%s: embedded copy differs from testdata", name)
		}
	}
}

func TestFixturesAreWellFormed(t *testing.T) {
	fsys := keyclient.Fixtures()
	for _, name := range exampleFiles {
		e := readExample(t, fsys, name)
		if e.Summary == "" || e.Description == "" {
			t.Errorf("%s: summary and description are required", name)
		}
		noBody := name == "keylist/not-modified.json" || name == "keyrequest/discovery-none.json"
		if noBody != (len(e.Value) == 0) {
			t.Errorf("%s: value present = %v, want %v", name, len(e.Value) != 0, !noBody)
		}
		if (name == "keyrequest/request.json") != (e.Status == 0) {
			t.Errorf("%s: x-status = %d", name, e.Status)
		}
		for _, s := range []string{e.Description, string(e.Value)} {
			if strings.Contains(s, "://") && !strings.Contains(s, "https://provider.example/") {
				t.Errorf("%s: a URL that is not on https://provider.example", name)
			}
		}
	}
}

// The round trip of testdata/README.md: the approved key is the first entry of
// the full list.
func TestFixturesRoundTripKey(t *testing.T) {
	fsys := keyclient.Fixtures()
	var approved struct{ Name, Role, Key string }
	if err := json.Unmarshal(readExample(t, fsys, "keyrequest/poll-approved.json").Value, &approved); err != nil {
		t.Fatal(err)
	}
	var list struct {
		Keys []struct{ Name, Role, SHA256 string } `json:"keys"`
	}
	if err := json.Unmarshal(readExample(t, fsys, "keylist/full.json").Value, &list); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(approved.Key))
	first := list.Keys[0]
	if first.SHA256 != hex.EncodeToString(sum[:]) || first.Name != approved.Name || first.Role != approved.Role {
		t.Fatalf("first list entry %+v does not match the approved key's hash, name and role", first)
	}
	notMod := readExample(t, fsys, "keylist/not-modified.json")
	full := readExample(t, fsys, "keylist/full.json")
	if notMod.Headers["ETag"] == "" || notMod.Headers["ETag"] != full.Headers["ETag"] {
		t.Fatalf("not-modified ETag %q, want the full list's %q", notMod.Headers["ETag"], full.Headers["ETag"])
	}
}

// The spec references every fixture that has a body, and every reference
// resolves.
func TestSpecReferencesFixtures(t *testing.T) {
	b, err := os.ReadFile("spec/key-request.yaml")
	if err != nil {
		t.Fatal(err)
	}
	refs := map[string]bool{}
	for _, m := range regexp.MustCompile(`\$ref: '\.\./testdata/([^']+)'`).FindAllStringSubmatch(string(b), -1) {
		refs[m[1]] = true
		if _, err := os.Stat("testdata/" + m[1]); err != nil {
			t.Errorf("spec references a missing fixture: %v", err)
		}
	}
	for _, name := range exampleFiles {
		e := readExample(t, keyclient.Fixtures(), name)
		if len(e.Value) > 0 && !refs[name] {
			t.Errorf("spec does not reference %s", name)
		}
	}
}
