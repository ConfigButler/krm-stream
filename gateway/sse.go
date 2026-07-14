package gateway

import (
	"context"
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

// SSESink writes protocol events to a connection as Server-Sent Events. It is safe for the heartbeat
// goroutine and the stream loop to use concurrently — which they must, since the whole point of a
// heartbeat is that it happens while nothing else is.
type SSESink struct {
	mu    sync.Mutex
	w     io.Writer
	flush func()
}

// NewSSESink writes to w, flushing after every frame if w can be flushed.
func NewSSESink(w io.Writer) *SSESink {
	s := &SSESink{w: w, flush: func() {}}
	if f, ok := w.(http.Flusher); ok {
		s.flush = f.Flush
	}
	return s
}

// Emit writes one event and flushes.
func (s *SSESink) Emit(_ context.Context, ev Event) error {
	frame, err := ev.MarshalSSE()
	if err != nil {
		return fmt.Errorf("krm-stream: marshal event: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.w.Write(frame); err != nil {
		return err
	}
	s.flush()
	return nil
}

// Comment writes an SSE comment. Comments are NOT events: a conforming consumer ignores them
// entirely, which is exactly why a heartbeat is one.
func (s *SSESink) Comment(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := fmt.Fprintf(s.w, ": %s\n\n", text); err != nil {
		return err
	}
	s.flush()
	return nil
}

// Heartbeat keeps the connection alive until ctx is done. Run it in a goroutine alongside the stream.
func (s *SSESink) Heartbeat(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Comment("heartbeat"); err != nil {
				return // the consumer went away; the stream loop will notice too
			}
		}
	}
}

// WriteSSEHeaders sets the response headers a conforming stream must carry, and must be called
// before the first frame.
func WriteSSEHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-KRM-Stream-Protocol", fmt.Sprint(ProtocolVersion))
	// A stream that a proxy is allowed to cache, buffer or transform is not a stream. `no-cache`
	// stops the browser; `no-transform` and the nginx-specific hint stop the middleboxes that
	// otherwise sit on the response until it is "big enough" — which turns a live status watch into
	// a batch job nobody can debug.
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// ServeStream runs one stream over one HTTP response, heartbeats included, and returns when the
// stream ends — at which point the caller returning from its handler closes the connection, which is
// what a terminal error requires.
func (g *Gateway) ServeStream(w http.ResponseWriter, r *http.Request, principal Principal, scope Scope) {
	g.ServeStreamProjection(w, r, principal, scope, "")
}

// ServeStreamProjection writes a stream using a caller-requested projection name. Projection
// authorization happens inside StreamProjection so direct callers and HTTP share the same rule.
func (g *Gateway) ServeStreamProjection(w http.ResponseWriter, r *http.Request, principal Principal, scope Scope, projection Projection) {
	WriteSSEHeaders(w)

	sink := NewSSESink(w)
	ctx := r.Context()

	hb, stopHeartbeat := context.WithCancel(ctx)
	interval := g.HeartbeatInterval
	if interval <= 0 {
		interval = HeartbeatInterval
	}
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		sink.Heartbeat(hb, interval)
	}()
	defer func() {
		stopHeartbeat()
		<-heartbeatDone
	}()

	// The error is already ON the wire by the time Stream returns — emitting it is how the consumer
	// learns anything. There is nothing left to tell the HTTP layer: the status line went out with
	// the very first byte, long before we could have known.
	_ = g.StreamProjection(ctx, principal, scope, projection, sink)
}
