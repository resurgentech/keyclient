//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package request_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/resurgentech/keyclient/keyclienttest"
	"github.com/resurgentech/keyclient/request"
)

// lockIsFree reports whether the lock of tokenPath can be taken now, by another
// open file description, as another process would.
func lockIsFree(t *testing.T, tokenPath string) bool {
	t.Helper()
	f, err := os.OpenFile(tokenPath+".lock", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return false
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return true
}

// T-16, T-I1: the background poll of a Wait false call keeps the lock until it
// ends.
func TestT16_BackgroundPollKeepsLock(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	ep := e.background(ctx)
	if lockIsFree(t, e.token) {
		t.Error("the lock is free while the background poll runs")
	}
	cancel()
	waitDone(t, ep)
	if !lockIsFree(t, e.token) {
		t.Error("the lock is still held after the background poll ended")
	}
	if !e.pendingExists() {
		t.Error("P removed on cancel")
	}
}

// T-22, T-I9: with another process holding the lock and P current, a Wait false
// call returns at once an *ErrPending built from P (marked resumed) and polls
// nothing; a goroutine waits for the lock, then follows T-11 (here: resumes and
// collects the key) without a second notice.
func TestT22_WaitFalseBehindLockReturnsPendingAtOnce(t *testing.T) {
	e := newEnv(t)
	first := fileAndStop(t, e)
	release := holdLock(t, e)
	var notices atomic.Int32
	var got request.Notice
	e.opts.Notify = func(n request.Notice) { notices.Add(1); got = n }
	e.opts.Wait = false
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	_, err := ensure(t, ctx, e.opts)
	var ep *request.ErrPending
	if !errors.As(err, &ep) {
		release()
		t.Fatalf("err = %v after %v, want *ErrPending at once", err, time.Since(start))
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("returned after %v", d)
	}
	if !ep.Notice.Resumed || ep.Notice.UserCode != first.UserCode || ep.Notice.ApproveURL != first.ApproveURL {
		t.Errorf("notice = %+v, want the one of P, resumed", ep.Notice)
	}
	if notices.Load() != 1 || got != ep.Notice {
		t.Errorf("Notify: %d calls, %+v", notices.Load(), got)
	}
	if n := len(e.p.Polls()); n != 0 {
		t.Errorf("polled %d times behind another's lock", n)
	}
	// A second caller in this process joins: same *ErrPending.
	_, err2 := ensure(t, ctx, e.opts)
	if ep2 := (*request.ErrPending)(nil); !errors.As(err2, &ep2) || ep2 != ep {
		t.Errorf("joiner: %v", err2)
	}
	e.p.Script(keyclienttest.Approved())
	release()
	waitDone(t, ep)
	checkReady(t, e.token, ep)
	if !ep.Ready() {
		t.Errorf("Err = %v", ep.Err())
	}
	if notices.Load() != 1 || len(e.p.Filed()) != 1 {
		t.Errorf("notices %d filed %d, want 1 and 1", notices.Load(), len(e.p.Filed()))
	}
}

// T-22: the goroutine that waited for the lock finds the token another holder
// stored (T-6); it never files.
func TestT22_BackgroundFindsTheToken(t *testing.T) {
	e := newEnv(t)
	fileAndStop(t, e)
	release := holdLock(t, e)
	e.opts.Wait = false
	_, err := ensure(t, context.Background(), e.opts)
	var ep *request.ErrPending
	if !errors.As(err, &ep) {
		release()
		t.Fatalf("err = %v", err)
	}
	if err := os.WriteFile(e.token, []byte("stored-by-the-holder-0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	release()
	waitDone(t, ep)
	checkReady(t, e.token, ep)
	if !ep.Ready() || len(e.p.Polls()) != 0 || len(e.p.Filed()) != 1 {
		t.Errorf("Err=%v polls=%d filed=%d", ep.Err(), len(e.p.Polls()), len(e.p.Filed()))
	}
	if e.pendingExists() {
		t.Error("T-27: the leftover P was not removed by the lock holder")
	}
}

// T-19 with T-22: a Wait true caller that joined the background flow of a Wait
// false call (waiting for another process's lock) does not get that flow's
// ErrBusy, a reason that is not its own: it starts over as owner, files and
// collects the key.
func TestT19_WaitTrueJoinerOfBusyBackgroundStartsOver(t *testing.T) {
	e := newEnv(t)
	fileAndStop(t, e)
	release := holdLock(t, e)
	o := e.opts
	o.Wait = false
	_, err := ensure(t, context.Background(), o)
	var ep *request.ErrPending
	if !errors.As(err, &ep) {
		release()
		t.Fatalf("err = %v, want *ErrPending", err)
	}
	type out struct {
		res request.Result
		err error
	}
	joined := make(chan out, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		res, err := ensure(t, ctx, e.opts) // Wait true: joins the background flow
		joined <- out{res, err}
	}()
	time.Sleep(200 * time.Millisecond)
	// The other process settles the request: P is gone when the lock is free,
	// so the background flow ends with ErrBusy.
	if err := os.Remove(request.PendingPath(e.token)); err != nil {
		t.Fatal(err)
	}
	e.p.Script(keyclienttest.Approved())
	release()
	waitDone(t, ep)
	if !errors.Is(ep.Err(), request.ErrBusy) {
		t.Errorf("background: Err = %v, want ErrBusy", ep.Err())
	}
	select {
	case got := <-joined:
		if got.err != nil || got.res.Token != e.p.Key() {
			t.Errorf("Wait true joiner: res=%+v err=%v, want it to start over and collect the key", got.res, got.err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the Wait true joiner hung")
	}
	if n := len(e.p.Filed()); n != 2 {
		t.Errorf("filed %d, want 2 (the first run, then the joiner as owner)", n)
	}
}

// T-22: the background goroutine resumes only the request it reported. If,
// when the lock is free, P holds another current request, it ends with ErrBusy
// and neither polls nor shows that other request.
func TestT22_BackgroundResumesOnlyTheReportedRequest(t *testing.T) {
	e := newEnv(t)
	first := fileAndStop(t, e)
	release := holdLock(t, e)
	var notices atomic.Int32
	e.opts.Notify = func(request.Notice) { notices.Add(1) }
	e.opts.Wait = false
	_, err := ensure(t, context.Background(), e.opts)
	var ep *request.ErrPending
	if !errors.As(err, &ep) || ep.Notice.UserCode != first.UserCode {
		release()
		t.Fatalf("err = %v, want the *ErrPending of the first request", err)
	}
	// The other process settles that request and files another one.
	writePending(t, e, 0o600, pendingJSON(e, time.Now().Add(time.Hour), e.p.URL()+"/v1/requests/old/poll",
		func(m map[string]any) { m["user_code"] = "CCCC-DDDD" }))
	release()
	waitDone(t, ep)
	if !errors.Is(ep.Err(), request.ErrBusy) {
		t.Errorf("background: Err = %v, want ErrBusy", ep.Err())
	}
	if n := len(e.p.Polls()); n != 0 {
		t.Errorf("the background polled %d times: it resumed a request it did not report", n)
	}
	if notices.Load() != 1 {
		t.Errorf("notices = %d, want 1", notices.Load())
	}
	if !e.pendingExists() {
		t.Error("the other request's P was removed")
	}
}

// T-22, T-I9: with another process holding the lock and no current P, a Wait
// false call returns ErrBusy at once; nothing is filed or shown.
func TestT22_WaitFalseBehindLockWithoutPendingIsBusy(t *testing.T) {
	for name, setup := range map[string]func(e *env){
		"no P": func(*env) {},
		"foreign P": func(e *env) {
			writePending(t, e, 0o600, pendingJSON(e, time.Now().Add(time.Hour), "http://127.0.0.1:1/poll", func(m map[string]any) { m["override"] = "https://other.example/r" }))
		},
		"expired P": func(e *env) {
			writePending(t, e, 0o600, pendingJSON(e, time.Now().Add(-time.Hour), "http://127.0.0.1:1/poll"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			setup(e)
			release := holdLock(t, e)
			defer release()
			notified := false
			e.opts.Notify = func(request.Notice) { notified = true }
			e.opts.Wait = false
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			start := time.Now()
			_, err := ensure(t, ctx, e.opts)
			if !errors.Is(err, request.ErrBusy) || time.Since(start) > time.Second {
				t.Errorf("err = %v after %v, want ErrBusy at once", err, time.Since(start))
			}
			if notified || len(e.p.Filed()) != 0 {
				t.Errorf("notified %v filed %d", notified, len(e.p.Filed()))
			}
		})
	}
}

// T-18, T-I9: a Wait false caller that joins an owner of this process which
// waits for another process's lock, with no notice to show, gets ErrBusy at once.
func TestT18_WaitFalseJoinerOfOwnerBehindLockIsBusy(t *testing.T) {
	e := newEnv(t)
	release := holdLock(t, e)
	ctx, cancel := context.WithCancel(context.Background())
	ownerDone := make(chan error, 1)
	go func() {
		_, err := request.Ensure(ctx, e.opts) // Wait true: waits for the lock
		ownerDone <- err
	}()
	time.Sleep(200 * time.Millisecond)
	o := e.opts
	o.Wait = false
	start := time.Now()
	_, err := ensure(t, context.Background(), o)
	if !errors.Is(err, request.ErrBusy) || time.Since(start) > time.Second {
		t.Errorf("joiner: err = %v after %v, want ErrBusy at once", err, time.Since(start))
	}
	cancel()
	release()
	<-ownerDone
}

// T-21: with another process holding the lock and P current, a Wait true call
// shows the notice of P once, marked resumed, while it waits; when the lock is
// freed it finds the token (T-6), and as the lock holder removes the leftover P
// (T-27).
func TestT21_WaitTrueBehindLockShowsNotice(t *testing.T) {
	e := newEnv(t)
	writePending(t, e, 0o600, pendingJSON(e, time.Now().Add(time.Hour), e.p.URL()+"/v1/requests/old/poll"))
	release := holdLock(t, e)
	noticed := make(chan request.Notice, 4)
	e.opts.Notify = func(n request.Notice) { noticed <- n }
	type out struct {
		res request.Result
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := ensure(t, context.Background(), e.opts)
		done <- out{res, err}
	}()
	select {
	case n := <-noticed:
		if !n.Resumed || n.UserCode != "AAAA-BBBB" {
			t.Errorf("notice = %+v", n)
		}
	case <-time.After(2 * time.Second):
		release()
		t.Fatal("no notice while waiting for the lock")
	}
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
	if len(noticed) != 0 {
		t.Errorf("%d more notices", len(noticed))
	}
	if e.pendingExists() {
		t.Error("T-27: P was not removed by the lock holder that found T")
	}
	if len(e.p.Polls()) != 0 || len(e.p.Filed()) != 0 {
		t.Error("the provider was contacted")
	}
}

// T-21 then T-11: the waiter resumes the request it showed, without a second
// notice.
func TestT21_WaitTrueBehindLockResumesWithoutSecondNotice(t *testing.T) {
	e := newEnv(t)
	fileAndStop(t, e)
	release := holdLock(t, e)
	var notices atomic.Int32
	e.opts.Notify = func(n request.Notice) {
		if notices.Add(1) == 1 {
			go func() { time.Sleep(100 * time.Millisecond); release() }()
		}
	}
	e.p.Script(keyclienttest.Approved())
	res, err := ensure(t, context.Background(), e.opts)
	if err != nil || res.Token != e.p.Key() {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if notices.Load() != 1 {
		t.Errorf("notices = %d, want 1", notices.Load())
	}
}

// T-28: a lock file that is a link, not a regular file, writable by group or
// others, or not owned by this user, is refused with an error that names it.
func TestT28_LockFileRefused(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, e *env){
		"symlink": func(t *testing.T, e *env) {
			os.Symlink(filepath.Join(filepath.Dir(e.token), "elsewhere"), e.token+".lock")
		},
		"directory": func(t *testing.T, e *env) { os.Mkdir(e.token+".lock", 0o700) },
		"group-writable": func(t *testing.T, e *env) {
			os.WriteFile(e.token+".lock", nil, 0o600)
			os.Chmod(e.token+".lock", 0o620)
		},
		"other-writable": func(t *testing.T, e *env) {
			os.WriteFile(e.token+".lock", nil, 0o600)
			os.Chmod(e.token+".lock", 0o602)
		},
		"not owned": func(t *testing.T, e *env) {
			t.Cleanup(request.SetGeteuid(func() int { return os.Geteuid() + 1 }))
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			if err := os.MkdirAll(filepath.Dir(e.token), 0o700); err != nil {
				t.Fatal(err)
			}
			setup(t, e)
			_, err := ensure(t, context.Background(), e.opts)
			if err == nil || !strings.Contains(err.Error(), e.token+".lock") {
				t.Errorf("err = %v, want an error that names the lock file", err)
			}
			if len(e.p.Filed()) != 0 {
				t.Error("filed")
			}
		})
	}
}

const t22Child = "KEYCLIENT_REQUEST_T22_CHILD"

// TestTI1_ChildPolls is process A of TestTI1_TwoProcessesOnePoller: Wait false,
// its background poll goes on while the test waits for it.
func TestTI1_ChildPolls(t *testing.T) {
	if os.Getenv(t22Child) == "" {
		t.Skip("run by TestTI1_TwoProcessesOnePoller")
	}
	t.Cleanup(request.SetSecond(unit))
	o := request.Options{
		Override: os.Getenv(t22Child + "_URL"), TokenPath: os.Getenv(t22Child + "_TOKEN"),
		Role: "agent", NameHint: "alice@laptop", Host: "laptop", Tool: "mytool/0.3.0", Wait: false,
	}
	_, err := request.Ensure(context.Background(), o)
	var ep *request.ErrPending
	if !errors.As(err, &ep) {
		fmt.Printf("CHILD failed: %v\n", err)
		return
	}
	fmt.Println("CHILD pending")
	select {
	case <-ep.Done():
	case <-time.After(30 * time.Second):
	}
	fmt.Printf("CHILD end ready=%v err=%v\n", ep.Ready(), ep.Err())
}

// startPoller starts process A and returns once its request is filed and its
// background poll runs.
func startPoller(t *testing.T, p *keyclienttest.Provider, token string) (wait func() string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestTI1_ChildPolls$", "-test.count=1")
	cmd.Env = append(os.Environ(), t22Child+"=1", t22Child+"_URL="+p.RequestURL(), t22Child+"_TOKEN="+token)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(stdout)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if sc.Text() == "CHILD pending" {
			break
		}
	}
	if len(lines) == 0 || lines[len(lines)-1] != "CHILD pending" {
		cmd.Wait()
		t.Fatalf("process A did not file: %q", lines)
	}
	return func() string {
		rest, _ := io.ReadAll(stdout)
		cmd.Wait()
		return strings.Join(lines, "\n") + "\n" + string(rest)
	}
}

// T-16, T-I1, T-I2, T-21, T-22 across two processes: while process A's
// background poll runs (Wait false, holding the lock), process B with Wait
// false returns *ErrPending from P at once and never polls; with Wait true it
// shows the notice of P (resumed), waits for the lock, then finds the token A
// stored. Only one process polls: every poll keeps the interval.
func TestTI1_TwoProcessesOnePoller(t *testing.T) {
	for _, wait := range []bool{false, true} {
		t.Run(fmt.Sprintf("Wait=%v", wait), func(t *testing.T) {
			e := newEnv(t)
			e.p.SetInterval(2)
			e.p.Script(keyclienttest.Pending(), keyclienttest.Pending(), keyclienttest.Pending(), keyclienttest.Pending(), keyclienttest.Approved())
			ct := &countingTransport{}
			e.opts.ServiceURL, e.opts.Override = "", e.p.RequestURL()
			e.opts.HTTPClient = &http.Client{Transport: ct}
			e.opts.Wait = wait
			var notices []request.Notice
			e.opts.Notify = func(n request.Notice) { notices = append(notices, n) }
			childOut := startPoller(t, e.p, e.token)

			start := time.Now()
			res, err := ensure(t, context.Background(), e.opts)
			if !wait {
				var ep *request.ErrPending
				if !errors.As(err, &ep) {
					t.Fatalf("B: err = %v, want *ErrPending", err)
				}
				if d := time.Since(start); d > time.Second {
					t.Errorf("B returned after %v, want at once", d)
				}
				if !ep.Notice.Resumed || ep.Notice.UserCode == "" {
					t.Errorf("B: notice %+v", ep.Notice)
				}
				waitDone(t, ep)
				checkReady(t, e.token, ep)
				if !ep.Ready() {
					t.Errorf("B: Err = %v", ep.Err())
				}
			} else if err != nil || res.Token != e.p.Key() {
				t.Errorf("B: res=%+v err=%v", res, err)
			}
			if len(notices) != 1 || !notices[0].Resumed {
				t.Errorf("B: notices %+v, want one, resumed", notices)
			}
			if n := ct.n.Load(); n != 0 {
				t.Errorf("B sent %d requests; it must neither poll nor file", n)
			}
			out := childOut()
			if !strings.Contains(out, "CHILD end ready=true") {
				t.Errorf("A:\n%s", out)
			}
			polls := e.p.Polls()
			for i := 1; i < len(polls); i++ {
				if gap := polls[i].Time.Sub(polls[i-1].Time); gap < 2*unit {
					t.Errorf("polls %d and %d are %v apart: two pollers", i-1, i, gap)
				}
			}
			if len(e.p.Filed()) != 1 {
				t.Errorf("filed %d", len(e.p.Filed()))
			}
		})
	}
}
