package main

import (
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"
)

// abortWriter stands in for a client that disconnects mid-stream: net/http's
// ResponseWriter starts returning an error on every write once the connection is
// gone.
type abortWriter struct{ writes int }

func (w *abortWriter) Write(p []byte) (int, error) {
	w.writes++
	return 0, errors.New("client disconnected")
}

// An SSE stream the client abandons must not strand the rewriter goroutine.
//
// The rewriter relays the upstream stream through an io.Pipe, which has no
// buffer: its goroutine blocks in a pipe write until the consumer reads. When the
// client disconnects, io.Copy stops reading and returns the write error, so
// without closing the pipe reader that goroutine blocks forever — and it holds
// the upstream response body open with it. Production suffered exactly this: one
// leaked goroutine and one leaked upstream connection per abandoned stream, for
// the life of the process.
func TestAnAbortedStreamDoesNotLeakTheRewriterGoroutine(t *testing.T) {
	// Warm up so the goroutine count is not measured against a cold runtime.
	sink := newResponseRewriter(strings.NewReader(""), nil)
	io.Copy(io.Discard, sink)
	sink.Close()

	before := runtime.NumGoroutine()

	// Long enough that the producer cannot finish before the consumer aborts.
	body := strings.Repeat("event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n", 2000)
	for i := 0; i < 20; i++ {
		rewritten := newResponseRewriter(strings.NewReader(body), nil)
		if _, err := io.Copy(&abortWriter{}, rewritten); err == nil {
			t.Fatal("the copy must fail once the client is gone")
		}
		rewritten.Close()
	}

	// Give the goroutines a moment to unwind, then count.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine()-before <= 2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("aborting 20 streams left %d extra goroutines running", runtime.NumGoroutine()-before)
}
