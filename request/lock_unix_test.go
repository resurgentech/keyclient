//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package request_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/resurgentech/keyclient/keyclienttest"
	"github.com/resurgentech/keyclient/request"
)

// holdLock takes the lock of e's token from outside the package, as another
// process would.
func holdLock(t *testing.T, e *env) (release func()) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(e.token), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(e.token+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	return func() { f.Close() }
}

func TestLockWaitHonoursContext(t *testing.T) {
	e := newEnv(t)
	release := holdLock(t, e)
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := request.Ensure(ctx, e.opts)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Errorf("err = %v after %v", err, time.Since(start))
	}
	if len(e.p.Filed()) != 0 {
		t.Error("filed while another holds the lock")
	}
}

// A caller that waited for the lock reads the token its holder stored.
func TestLockWaiterReadsTheToken(t *testing.T) {
	e := newEnv(t)
	release := holdLock(t, e)
	type out struct {
		res request.Result
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := request.Ensure(context.Background(), e.opts)
		done <- out{res, err}
	}()
	time.Sleep(100 * time.Millisecond)
	const tok = "stored-by-the-holder-0123456789"
	if err := os.WriteFile(e.token, []byte(tok), 0o600); err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case got := <-done:
		if got.err != nil || got.res != (request.Result{Token: tok}) {
			t.Errorf("res=%+v err=%v", got.res, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter did not get the lock")
	}
	if len(e.p.Filed()) != 0 {
		t.Error("filed")
	}
}

func TestLockFileNotFollowed(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Dir(e.token)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "elsewhere")
	if err := os.Symlink(target, e.token+".lock"); err != nil {
		t.Fatal(err)
	}
	_, err := request.Ensure(context.Background(), e.opts)
	if err == nil || len(e.p.Filed()) != 0 {
		t.Errorf("err = %v, filed = %d", err, len(e.p.Filed()))
	}
	if _, serr := os.Lstat(target); serr == nil {
		t.Error("the link was followed: its target was created")
	}
}

func TestPendingFIFORefused(t *testing.T) {
	e := newEnv(t)
	if err := os.MkdirAll(filepath.Dir(e.token), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(request.PendingPath(e.token), 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := request.Ensure(context.Background(), e.opts)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), request.PendingPath(e.token)) {
			t.Errorf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Ensure blocked on a FIFO at the pending path")
	}
	if len(e.p.Filed()) != 0 {
		t.Error("filed")
	}
}

const childEnv = "KEYCLIENT_REQUEST_TEST_CHILD"

// TestTwoProcessesChild is one of the processes of TestTwoProcesses.
func TestTwoProcessesChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip("run by TestTwoProcesses")
	}
	t.Cleanup(request.SetSecond(unit))
	o := request.Options{
		Override: os.Getenv(childEnv + "_URL"), TokenPath: os.Getenv(childEnv + "_TOKEN"),
		Role: "agent", NameHint: "alice@laptop", Host: "laptop", Tool: "mytool/0.3.0", Wait: true,
		Notify: func(request.Notice) { fmt.Println("CHILD notice") },
	}
	res, err := request.Ensure(context.Background(), o)
	if err == nil && res.Token == os.Getenv(childEnv+"_KEY") {
		fmt.Println("CHILD has the token")
	} else {
		fmt.Printf("CHILD failed: %v\n", err)
	}
}

// Two processes started together with no token and no pending file file one
// request, and both end with the token (reviewer R8).
func TestTwoProcesses(t *testing.T) {
	p := keyclienttest.NewProvider(t)
	p.SetInterval(2)
	p.Script(keyclienttest.Pending(), keyclienttest.Approved())
	token := filepath.Join(t.TempDir(), "conf", "token")
	outs := make([]string, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestTwoProcessesChild$", "-test.count=1")
			cmd.Env = append(os.Environ(), childEnv+"=1", childEnv+"_URL="+p.RequestURL(),
				childEnv+"_TOKEN="+token, childEnv+"_KEY="+p.Key())
			b, err := cmd.CombinedOutput()
			if err != nil {
				t.Errorf("child %d: %v\n%s", i, err, b)
			}
			outs[i] = string(b)
		}()
	}
	wg.Wait()
	notices := 0
	for i, out := range outs {
		has := false
		sc := bufio.NewScanner(strings.NewReader(out))
		for sc.Scan() {
			switch line := sc.Text(); {
			case line == "CHILD notice":
				notices++
			case line == "CHILD has the token":
				has = true
			case strings.HasPrefix(line, "CHILD"):
				t.Logf("child %d: %s", i, line)
			}
		}
		if !has {
			t.Errorf("child %d did not end with the token:\n%s", i, out)
		}
	}
	if n := len(p.Filed()); n != 1 || notices != 1 {
		t.Errorf("filed %d, notices %d; want 1 and 1", n, notices)
	}
}
