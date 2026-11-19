# keyclient: the client side of the key-request and key-list contracts. Native justfile.

# Build and test scratch stays inside the repo (tmp/ is gitignored).
export GOTMPDIR := justfile_directory() / "tmp" / "go"

# List available recipes
default:
  @just --list

# Run every test with the race detector, uncached (test temp dirs under tmp/ too)
test *args: _scratch
  TMPDIR="$GOTMPDIR" go test -race -count=1 {{args}} ./...

# go vet, gofmt, and no dependencies outside the standard library
vet: _scratch _no-require
  go vet ./...
  @files="$(find . -path ./tmp -prune -o -name '*.go' -print | xargs gofmt -l)"; test -z "$files" || { echo "gofmt needed: $files"; exit 1; }

# vet, plus go.mod is tidy and the module list is this module alone
lint: vet
  go mod tidy -diff
  @test "$(go list -m all)" = "github.com/resurgentech/keyclient" || { echo "go list -m all shows more than this module:"; go list -m all; exit 1; }

# go.mod must have no require: the module is standard library only
_no-require:
  @! grep -q '^[[:space:]]*require' go.mod || { echo "go.mod has a require; keyclient is standard library only"; exit 1; }

_scratch:
  @mkdir -p "$GOTMPDIR"
