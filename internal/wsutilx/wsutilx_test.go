package wsutilx

import (
	"context"
	"net"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestSetReadDeadline_TimesOut verifies that a read deadline set via the
// helper causes a hung Read to return ErrDeadlineExceeded within the
// expected window.
func TestSetReadDeadline_TimesOut(t *testing.T) {
	cli, srv := net.Pipe()
	defer cli.Close()
	defer srv.Close()

	SetReadDeadline(cli, 100*time.Millisecond)
	buf := make([]byte, 8)
	start := time.Now()
	_, err := cli.Read(buf)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !os.IsTimeout(err) {
		t.Fatalf("expected timeout, got %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("Read took %v, expected <500ms", elapsed)
	}
}

// TestWatchCancel_PushesDeadlineOnCtxDone verifies that cancelling the
// context unblocks an in-flight Read on the watched conn.
func TestWatchCancel_PushesDeadlineOnCtxDone(t *testing.T) {
	cli, srv := net.Pipe()
	defer cli.Close()
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	stop := WatchCancel(ctx, cli)
	defer stop()

	var (
		readErr error
		elapsed time.Duration
		done    = make(chan struct{})
	)
	go func() {
		start := time.Now()
		buf := make([]byte, 8)
		_, readErr = cli.Read(buf)
		elapsed = time.Since(start)
		close(done)
	}()

	// Let the read settle into its blocked state, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not unblock within 2s of ctx cancel")
	}
	if readErr == nil {
		t.Fatal("expected error after ctx cancel, got nil")
	}
	if !os.IsTimeout(readErr) {
		t.Fatalf("expected timeout error, got %v", readErr)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("Read unblocked after %v, expected ~50ms", elapsed)
	}
}

// TestWatchCancel_StopReleasesWatcher verifies the stop function terminates
// the watcher goroutine when ctx never fires.
func TestWatchCancel_StopReleasesWatcher(t *testing.T) {
	cli, srv := net.Pipe()
	defer cli.Close()
	defer srv.Close()

	ctx := context.Background() // never cancelled
	before := runtime.NumGoroutine()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stop := WatchCancel(ctx, cli)
			stop()
		}()
	}
	wg.Wait()
	// Allow a brief window for the watcher goroutines to schedule out.
	time.Sleep(100 * time.Millisecond)

	after := runtime.NumGoroutine()
	if after-before > 4 {
		t.Errorf("goroutines leaked: before=%d after=%d", before, after)
	}
}

// TestWatchCancel_NilCtxIsNoop verifies the helper is safe to call with a
// nil context (returns a no-op stop fn, no goroutine spawned).
func TestWatchCancel_NilCtxIsNoop(t *testing.T) {
	cli, srv := net.Pipe()
	defer cli.Close()
	defer srv.Close()

	stop := WatchCancel(nil, cli)
	stop() // must not panic
}
