package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// The transport. Everything above this file is the protocol; this is the only part that knows what a
// byte is.
//
// It is small, and every line of it is a rule from spec §7:
//
//   - `text/event-stream`, one JSON object per `data:` frame.
//   - A `: heartbeat` COMMENT every ~20s. Not an event — a consumer ignores it — but without it an
//     intermediary idles the connection out and the browser silently stops receiving `status`.
//   - NO `id:` lines. Ever. v1 has no delta replay and no Last-Event-ID resume, and putting a
//     resource uid there (the tempting thing) gives the browser's automatic reconnect an entirely
//     incorrect meaning.
//   - A terminal error is the LAST event, and then the connection closes — because EventSource
//     reconnects on its own otherwise, and would hammer a forbidden scope forever.
//   - Flush after every frame. An unflushed SSE stream is a stream that arrives when the buffer
//     happens to fill, which for a live status watch is indistinguishable from being broken.

// HeartbeatInterval is how often a quiet connection is kept alive. ~20s: comfortably under the 30–60s
// idle timeout of every proxy anyone actually deploys behind.
const HeartbeatInterval = 20 * time.Second

// SSESink frames events for an io.Writer. Calls are serialized. Generic writers have
// no library-installed deadline; HTTP callers should use Gateway.ServeStream.
type SSESink struct {
	mu     sync.Mutex
	w      io.Writer
	flush  func() error
	before func(context.Context) error
	after  func() error
	cancel context.CancelFunc
	failed error
}

// NewSSESink writes to w, flushing after every frame when supported. A FlushError
// method takes precedence over http.Flusher so reported failures reach the caller.
func NewSSESink(w io.Writer) *SSESink {
	s := &SSESink{w: w, flush: func() error { return nil }}
	switch f := w.(type) {
	case interface{ FlushError() error }:
		s.flush = f.FlushError
	case http.Flusher:
		s.flush = func() error { f.Flush(); return nil }
	}
	return s
}

// operation serializes the entire deadline/write/flush/clear operation. Failures
// poison the sink so a queued heartbeat or terminal event cannot revive it.
func (s *SSESink) operation(ctx context.Context, write func() error) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed != nil {
		return s.failed
	}
	defer func() {
		if err != nil {
			s.failed = err
			if s.cancel != nil {
				s.cancel()
			}
		}
	}()
	if err = ctx.Err(); err != nil {
		return err
	}
	if s.before != nil {
		if err = s.before(ctx); err != nil {
			return err
		}
	}
	if err = write(); err != nil {
		return err
	}
	if err = s.flush(); err != nil {
		return err
	}
	if s.after != nil {
		return s.after()
	}
	return nil
}

func (s *SSESink) write(ctx context.Context, frame []byte) error {
	return s.operation(ctx, func() error {
		n, err := s.w.Write(frame)
		if err == nil && n != len(frame) {
			return io.ErrShortWrite
		}
		return err
	})
}

// Emit writes and flushes one event.
func (s *SSESink) Emit(ctx context.Context, ev Event) error {
	frame, err := ev.MarshalSSE()
	if err != nil {
		return fmt.Errorf("krm-stream: marshal event: %w", err)
	}
	return s.write(ctx, frame)
}

// Comment writes an SSE comment, not a protocol event.
func (s *SSESink) Comment(text string) error {
	return s.write(context.Background(), []byte(": "+text+"\n\n"))
}

// Heartbeat runs until cancellation or an I/O failure. Its owner must handle the
// returned error and stop the stream on failure. Nonpositive intervals use the default.
func (s *SSESink) Heartbeat(ctx context.Context, every time.Duration) error {
	if every <= 0 {
		every = HeartbeatInterval
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := s.write(ctx, []byte(": heartbeat\n\n")); err != nil {
				return err
			}
		}
	}
}

// CheckHTTPStreaming checks flush and write-deadline capabilities without writing
// or flushing a response. It follows Unwrap methods like http.ResponseController.
// The deadline probe clears any existing write deadline. Call before streaming,
// with no concurrent use of w. This checks exposed capabilities, not middleware
// correctness or proxy buffering. Unsupported operations wrap http.ErrNotSupported.
func CheckHTTPStreaming(w http.ResponseWriter) error {
	for current := w; ; {
		switch v := current.(type) {
		case interface{ FlushError() error }:
			return checkWriteDeadline(w)
		case http.Flusher:
			return checkWriteDeadline(w)
		case interface{ Unwrap() http.ResponseWriter }:
			current = v.Unwrap()
		default:
			return fmt.Errorf("krm-stream: HTTP flush: %w", http.ErrNotSupported)
		}
	}
}

func checkWriteDeadline(w http.ResponseWriter) error {
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
		return fmt.Errorf("krm-stream: HTTP write deadline: %w", err)
	}
	return nil
}

func writeSSEHeaders(ctx context.Context, w http.ResponseWriter, s *SSESink) error {
	return s.operation(ctx, func() error {
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("X-KRM-Stream-Protocol", fmt.Sprint(ProtocolVersion))
		h.Set("Cache-Control", "no-cache, no-transform")
		h.Set("Connection", "keep-alive")
		h.Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		return nil
	})
}

// ServeStream runs one HTTP stream, including headers, heartbeats and cleanup.
func (g *Gateway) ServeStream(w http.ResponseWriter, r *http.Request, principal Principal, scope Scope) {
	g.ServeStreamProjection(w, r, principal, scope, "")
}

// ServeStreamProjection serves a requested projection under the gateway's policy.
// With WriteTimeout enabled, unsupported writers abort before a stream opens.
func (g *Gateway) ServeStreamProjection(w http.ResponseWriter, r *http.Request, principal Principal, scope Scope, projection Projection) {
	g.serveHTTP(w, r, func(ctx context.Context, sink *SSESink) {
		// A terminal frame may itself fail; delivery is not guaranteed on a failed transport.
		_ = g.StreamProjection(ctx, principal, scope, projection, sink)
	})
}

func (g *Gateway) serveHTTP(w http.ResponseWriter, r *http.Request, run func(context.Context, *SSESink)) {
	if g.WriteTimeout < 0 {
		panic("krm-stream: WriteTimeout must not be negative")
	}
	if g.WriteTimeout > 0 {
		if err := CheckHTTPStreaming(w); err != nil {
			g.observe(Observation{Kind: ObservationHTTPTransportRejected, Code: CodeInternal})
			panic(http.ErrAbortHandler)
		}
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	sink := NewSSESink(w)
	sink.cancel = cancel
	controller := http.NewResponseController(w)
	// Zero timeout retains optional flushing, including transparent HTTP wrappers.
	sink.flush = func() error {
		err := controller.Flush()
		if g.WriteTimeout == 0 && errors.Is(err, http.ErrNotSupported) {
			return nil
		}
		return err
	}
	if g.WriteTimeout > 0 {
		sink.before = func(ctx context.Context) error {
			deadline := time.Now().Add(g.WriteTimeout)
			if earlier, ok := ctx.Deadline(); ok && earlier.Before(deadline) {
				deadline = earlier
			}
			return controller.SetWriteDeadline(deadline)
		}
		sink.after = func() error { return controller.SetWriteDeadline(time.Time{}) }
		// Runs after the heartbeat has exited. Clearing never revives a failed response.
		defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	}
	if err := writeSSEHeaders(ctx, w, sink); err != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := sink.Heartbeat(ctx, g.HeartbeatInterval); err != nil {
			cancel()
		}
	}()
	defer func() { cancel(); <-done }()
	run(ctx, sink)
}
