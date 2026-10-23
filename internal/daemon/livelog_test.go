package daemon

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf is a bytes.Buffer that a test goroutine can read while another one writes.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestLiveLogSnapshotAndFollow(t *testing.T) {
	l := newLiveLog()
	l.Write([]byte("one\ntwo\n"))

	var snap bytes.Buffer
	if err := l.Stream(context.Background(), &snap, false, 0, nil); err != nil || snap.String() != "one\ntwo\n" {
		t.Fatalf("snapshot: %q %v", snap.String(), err)
	}

	var got syncBuf
	done := make(chan error, 1)
	flushes := 0
	var fm sync.Mutex
	go func() {
		done <- l.Stream(context.Background(), &got, true, 0, func() { fm.Lock(); flushes++; fm.Unlock() })
	}()
	waitFor(t, "the backlog", func() bool { return got.String() == "one\ntwo\n" })
	l.Write([]byte("three\n"))
	waitFor(t, "new output", func() bool { return got.String() == "one\ntwo\nthree\n" })
	select {
	case <-done:
		t.Fatal("a followed stream must stay open while the build runs")
	default:
	}
	if !l.Running() {
		t.Error("Running before Close")
	}
	l.Write([]byte("four\n"))
	l.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got.String() != "one\ntwo\nthree\nfour\n" || l.Running() {
		t.Errorf("after close: %q running=%v", got.String(), l.Running())
	}
	fm.Lock()
	defer fm.Unlock()
	if flushes < 3 {
		t.Errorf("every chunk must be flushed to the client, flushes = %d", flushes)
	}
	// closing twice and streaming a finished log are fine
	l.Close()
	var again bytes.Buffer
	if err := l.Stream(context.Background(), &again, true, 0, nil); err != nil || again.String() != got.String() {
		t.Errorf("finished log with follow returns at once: %q %v", again.String(), err)
	}
}

func TestLiveLogManyFollowers(t *testing.T) {
	l := newLiveLog()
	var wg sync.WaitGroup
	bufs := make([]*syncBuf, 5)
	for i := range bufs {
		bufs[i] = &syncBuf{}
		wg.Add(1)
		go func(b *syncBuf) {
			defer wg.Done()
			l.Stream(context.Background(), b, true, 0, nil)
		}(bufs[i])
	}
	for i := 0; i < 50; i++ {
		l.Write([]byte("line\n"))
	}
	l.Close()
	wg.Wait()
	for i, b := range bufs {
		if strings.Count(b.String(), "line\n") != 50 {
			t.Errorf("follower %d got %d lines", i, strings.Count(b.String(), "line\n"))
		}
	}
}

func TestLiveLogFollowStopsWithTheContext(t *testing.T) {
	l := newLiveLog()
	l.Write([]byte("x\n"))
	ctx, cancel := context.WithCancel(context.Background())
	var got syncBuf
	done := make(chan error, 1)
	go func() { done <- l.Stream(ctx, &got, true, 0, nil) }()
	waitFor(t, "backlog", func() bool { return got.String() == "x\n" })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

func TestLiveLogRetentionDropsTheOldestAndKeepsOffsetsRight(t *testing.T) {
	l := newLiveLog()
	chunk := strings.Repeat("a", 1000) + "\n"
	n := MaxLiveLog/len(chunk) + 50
	for i := 0; i < n; i++ {
		l.Write([]byte(chunk))
	}
	l.Write([]byte("the end\n"))
	l.Close()
	var out bytes.Buffer
	l.Stream(context.Background(), &out, false, 0, nil)
	if out.Len() > MaxLiveLog || !strings.HasSuffix(out.String(), "the end\n") || out.Len() < MaxLiveLog-len(chunk)-100 {
		t.Errorf("retained %d bytes (cap %d)", out.Len(), MaxLiveLog)
	}
	if l.base == 0 {
		t.Error("the dropped bytes must be accounted for in the base offset")
	}

	// a follower that is slower than the retention window skips ahead instead of failing
	l2 := newLiveLog()
	var slow syncBuf
	l2.Write([]byte("start\n"))
	started := make(chan struct{})
	go func() {
		// read the backlog, then block on a writer that holds the buffer
		l2.Stream(context.Background(), &blockingWriter{inner: &slow, first: started}, true, 0, nil)
	}()
	<-started
	for i := 0; i < n; i++ {
		l2.Write([]byte(chunk))
	}
	l2.Write([]byte("tail\n"))
	l2.Close()
	waitFor(t, "the slow follower to catch up", func() bool { return strings.HasSuffix(slow.String(), "tail\n") })
}

// blockingWriter signals after its first write and then stalls until the test is done writing.
type blockingWriter struct {
	inner *syncBuf
	first chan struct{}
	once  sync.Once
}

func (b *blockingWriter) Write(p []byte) (int, error) {
	n, err := b.inner.Write(p)
	b.once.Do(func() { close(b.first); time.Sleep(100 * time.Millisecond) })
	return n, err
}

func TestTailLines(t *testing.T) {
	l := newLiveLog()
	l.Write([]byte("1\n2\n3\n4\n5\n"))
	l.Close()
	tail := func(n int) string {
		var b bytes.Buffer
		l.Stream(context.Background(), &b, false, n, nil)
		return b.String()
	}
	for n, want := range map[int]string{0: "1\n2\n3\n4\n5\n", 1: "5\n", 2: "4\n5\n", 5: "1\n2\n3\n4\n5\n", 99: "1\n2\n3\n4\n5\n"} {
		if got := tail(n); got != want {
			t.Errorf("tail %d = %q, want %q", n, got, want)
		}
	}
	// no trailing newline: the last (partial) line still counts as a line
	l2 := newLiveLog()
	l2.Write([]byte("a\nb\npartial"))
	var b bytes.Buffer
	l2.Stream(context.Background(), &b, false, 1, nil)
	if b.String() != "partial" {
		t.Errorf("partial last line: %q", b.String())
	}
	var s bytes.Buffer
	storedLog{"x\ny\nz\n"}.Stream(context.Background(), &s, true, 2, nil)
	if s.String() != "y\nz\n" {
		t.Errorf("stored log tail: %q", s.String())
	}
}
