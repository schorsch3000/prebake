package daemon

import (
	"bytes"
	"context"
	"io"
	"sync"
)

// MaxLiveLog bounds what is kept of one build's output in memory (the oldest part is dropped).
const MaxLiveLog = 512 << 10

// LogStream is the build output of one job, as the API serves it.
type LogStream interface {
	// Stream writes the retained output to w, last `tail` lines only if tail > 0, and with
	// follow keeps writing new output until the build ends or ctx does. flush is called
	// after every write so that a streaming HTTP response reaches the client at once.
	Stream(ctx context.Context, w io.Writer, follow bool, tail int, flush func()) error
}

// LiveLog collects the output of a running build and lets any number of readers follow it.
type LiveLog struct {
	mu     sync.Mutex
	buf    []byte        // retained output
	base   int64         // total bytes dropped from the front: buf[0] is offset base
	done   bool          // the build ended
	change chan struct{} // closed and replaced on every write and on Close
}

func newLiveLog() *LiveLog { return &LiveLog{change: make(chan struct{})} }

func (l *LiveLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	l.buf = append(l.buf, p...)
	if over := len(l.buf) - MaxLiveLog; over > 0 {
		l.buf = append([]byte(nil), l.buf[over:]...)
		l.base += int64(over)
	}
	close(l.change)
	l.change = make(chan struct{})
	l.mu.Unlock()
	return len(p), nil
}

// Close marks the build as ended and wakes the followers.
func (l *LiveLog) Close() {
	l.mu.Lock()
	if !l.done {
		l.done = true
		close(l.change)
		l.change = make(chan struct{})
	}
	l.mu.Unlock()
}

// Running reports whether the build is still producing output.
func (l *LiveLog) Running() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.done
}

// lastLines returns the offset (relative to buf) where the last n lines start.
func lastLines(buf []byte, n int) int {
	if n <= 0 {
		return 0
	}
	trimmed := bytes.TrimRight(buf, "\n")
	i := len(trimmed)
	for ; n > 0 && i > 0; n-- {
		i = bytes.LastIndexByte(trimmed[:i], '\n')
		if i < 0 {
			return 0
		}
	}
	return i + 1
}

func (l *LiveLog) Stream(ctx context.Context, w io.Writer, follow bool, tail int, flush func()) error {
	l.mu.Lock()
	off := l.base + int64(lastLines(l.buf, tail))
	l.mu.Unlock()
	for {
		l.mu.Lock()
		if off < l.base { // the reader fell behind the retention window
			off = l.base
		}
		chunk := append([]byte(nil), l.buf[off-l.base:]...)
		off += int64(len(chunk))
		done, change := l.done, l.change
		l.mu.Unlock()
		if len(chunk) > 0 {
			if _, err := w.Write(chunk); err != nil {
				return err
			}
			if flush != nil {
				flush()
			}
		}
		if done || !follow {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-change:
		}
	}
}

// storedLog is the output of a finished job that was read back from the state database
// (after a restart of the daemon).
type storedLog struct{ text string }

func (s storedLog) Stream(_ context.Context, w io.Writer, _ bool, tail int, flush func()) error {
	b := []byte(s.text)
	if _, err := w.Write(b[lastLines(b, tail):]); err != nil {
		return err
	}
	if flush != nil {
		flush()
	}
	return nil
}
