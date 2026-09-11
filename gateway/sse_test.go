package gateway

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type transparentWriter struct{ http.ResponseWriter }

func (w transparentWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type opaqueWriter struct{ http.ResponseWriter }

func TestCheckHTTPStreamingMountedMiddleware(t *testing.T) {
	for _, opaque := range []bool{false, true} {
		t.Run(fmt.Sprint(opaque), func(t *testing.T) {
			checked := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if opaque {
					w = opaqueWriter{w}
				} else {
					w = transparentWriter{w}
				}
				checked <- CheckHTTPStreaming(w)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			res, err := server.Client().Get(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			_ = res.Body.Close()
			err = <-checked
			if opaque != errors.Is(err, http.ErrNotSupported) {
				t.Fatalf("capability error = %v", err)
			}
		})
	}
	if err := CheckHTTPStreaming(httptest.NewRecorder()); !errors.Is(err, http.ErrNotSupported) {
		t.Fatal(err)
	}
}

func TestUnsupportedTransportHasNoStreamLifetime(t *testing.T) {
	var observations []Observation
	g := &Gateway{WriteTimeout: time.Second, Observer: ObserverFunc(func(o Observation) { observations = append(observations, o) })}
	func() {
		defer func() {
			if got := recover(); got != http.ErrAbortHandler {
				t.Fatalf("panic = %v", got)
			}
		}()
		g.ServeStream(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil), nil, Scope{})
	}()
	if len(observations) != 1 || observations[0].Kind != ObservationHTTPTransportRejected {
		t.Fatalf("observations = %v", observations)
	}
}

type failingHTTPWriter struct {
	header          http.Header
	failFlush       int
	flushes, writes int
	deadlines       []time.Time
	short           bool
}

func (w *failingHTTPWriter) Header() http.Header { return w.header }
func (*failingHTTPWriter) WriteHeader(int)       {}
func (w *failingHTTPWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.short {
		return len(p) - 1, nil
	}
	return len(p), nil
}
func (w *failingHTTPWriter) FlushError() error {
	w.flushes++
	if w.flushes == w.failFlush {
		return io.ErrClosedPipe
	}
	return nil
}
func (w *failingHTTPWriter) SetWriteDeadline(d time.Time) error {
	w.deadlines = append(w.deadlines, d)
	return nil
}

type transportBackend struct {
	events  chan WatchEvent
	stopped chan struct{}
	once    sync.Once
}

func newTransportBackend() *transportBackend {
	return &transportBackend{events: make(chan WatchEvent, 8), stopped: make(chan struct{})}
}
func (b *transportBackend) Watch(context.Context, Scope) (Watcher, error) { return b, nil }
func (b *transportBackend) Next(ctx context.Context) (WatchEvent, error) {
	select {
	case ev := <-b.events:
		return ev, nil
	case <-ctx.Done():
		return WatchEvent{}, ctx.Err()
	}
}
func (b *transportBackend) Stop() { b.once.Do(func() { close(b.stopped) }) }
func transportGateway(b Backend) *Gateway {
	return &Gateway{Auth: AllowAll{}, Clients: func(context.Context, string, Principal) (Backend, error) { return b, nil }, WriteTimeout: 300 * time.Millisecond, HeartbeatInterval: 20 * time.Millisecond}
}
func waitTransport(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(8 * time.Second):
		t.Fatal("transport cleanup did not complete")
	}
}

func TestHTTPFailuresCancelQuietStream(t *testing.T) {
	for _, fail := range []int{1, 2, 3} { // headers, reset, heartbeat (no upstream events)
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			b := newTransportBackend()
			w := &failingHTTPWriter{header: make(http.Header), failFlush: fail}
			g := transportGateway(b)
			done := make(chan struct{})
			go func() {
				defer close(done)
				g.ServeStream(w, httptest.NewRequest("GET", "/", nil), nil, sharedScopeUnderTest)
			}()
			waitTransport(t, done)
			if fail > 1 {
				waitTransport(t, b.stopped)
			}
			if w.flushes != fail {
				t.Fatalf("wrote after failure: %d flushes", w.flushes)
			}
			if !w.deadlines[len(w.deadlines)-1].IsZero() {
				t.Fatal("deadline not cleared on exit")
			}
		})
	}
}

func TestSinkShortWriteAndFailedTerminalDelivery(t *testing.T) {
	w := &failingHTTPWriter{header: make(http.Header), short: true}
	s := NewSSESink(w)
	if err := s.Comment("x"); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
	_ = s.Comment("again")
	if w.writes != 1 || w.flushes != 0 {
		t.Fatal("failed sink was reused")
	}
	var kinds []ObservationKind
	g := &Gateway{Auth: AuthorizerFunc(func(context.Context, Principal, Scope) error { return Forbidden("denied") }), Observer: ObserverFunc(func(o Observation) { kinds = append(kinds, o.Kind) })}
	if err := g.Stream(t.Context(), nil, Scope{}, s); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
	if fmt.Sprint(kinds) != "[stream_opened terminal_error stream_closed]" {
		t.Fatal(kinds)
	}
}

// Exhaust real socket / HTTP2 stream flow-control buffers. The healthy HTTP2
// subscriber shares the same client transport and connection with the stalled one.
func TestBlockedHTTPSubscriberReleasesSharedWatch(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		for _, trigger := range []string{"write-timeout", "expiry", "revocation"} {
			t.Run(fmt.Sprintf("http2=%t/%s", h2, trigger), func(t *testing.T) {
				b := newTransportBackend()
				shared := NewSharedBackend(b)
				g := transportGateway(shared)
				g.WriteTimeout = 2 * time.Second
				g.ReauthorizationInterval = 20 * time.Millisecond
				g.ReauthorizationTimeout = 2 * time.Second
				var revoked atomic.Bool
				g.Auth = AuthorizerFunc(func(_ context.Context, p Principal, _ Scope) error {
					if p == "slow" && revoked.Load() {
						return Forbidden("revoked")
					}
					return nil
				})
				slowDone, fastDone := make(chan struct{}), make(chan struct{})
				slowCtx, expire := context.WithCancel(t.Context())
				defer expire()
				var conns atomic.Int32
				srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					who := strings.TrimPrefix(r.URL.Path, "/")
					if who == "slow" {
						defer close(slowDone)
						r = r.WithContext(slowCtx)
					} else {
						defer close(fastDone)
					}
					g.ServeStream(w, r, who, sharedScopeUnderTest)
				}))
				srv.EnableHTTP2 = h2
				srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
					if state == http.StateNew {
						conns.Add(1)
					}
				}
				if h2 {
					srv.StartTLS()
				} else {
					srv.Start()
				}
				defer srv.Close()
				client := srv.Client()
				client.Timeout = 5 * time.Second
				fast, err := client.Get(srv.URL + "/fast")
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = fast.Body.Close() }()
				fastEvents := make(chan string, 16)
				// The test closes this body itself once isolation is proven; the read error that
				// causes is the test ending, not the healthy stream failing.
				var fastClosing atomic.Bool
				readerDone := make(chan struct{})
				go func() {
					defer close(readerDone)
					scanner := bufio.NewScanner(fast.Body)
					scanner.Buffer(make([]byte, 4096), 32<<20)
					for scanner.Scan() {
						line := scanner.Text()
						if strings.HasPrefix(line, "data:") {
							fastEvents <- line
						}
					}
					if err := scanner.Err(); err != nil && !fastClosing.Load() {
						t.Errorf("healthy reader: %v", err)
					}
					close(fastEvents)
				}()
				if h2 {
					slow, err := client.Get(srv.URL + "/slow")
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = slow.Body.Close() }()
					if slow.ProtoMajor != 2 {
						t.Fatal("HTTP2 not negotiated")
					}
				} else {
					conn, err := net.Dial("tcp", srv.Listener.Addr().String())
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = conn.Close() }()
					if tcp, ok := conn.(*net.TCPConn); ok {
						_ = tcp.SetReadBuffer(1024)
					}
					if _, err := fmt.Fprintf(conn, "GET /slow HTTP/1.1\r\nHost: test\r\n\r\n"); err != nil {
						t.Fatal(err)
					}
				}
				// Wait for both attachments before publishing the snapshot.
				deadline := time.Now().Add(time.Second)
				for {
					shared.mu.Lock()
					ss := shared.scopes[scopeKey(sharedScopeUnderTest)]
					count := 0
					if ss != nil {
						ss.mu.Lock()
						count = len(ss.subs)
						ss.mu.Unlock()
					}
					shared.mu.Unlock()
					if count == 2 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("subscribers did not attach")
					}
					time.Sleep(time.Millisecond)
				}
				big := obj("big", "large", "1")
				big["data"] = map[string]any{"payload": strings.Repeat("x", 16<<20)}
				b.events <- WatchEvent{Type: WatchAdded, Object: big}
				b.events <- WatchEvent{Type: WatchBookmark, InitialEventsEnd: true}
				// Start the trigger after delivery has begun; the frame exceeds socket and H2 buffers.
				waitFast := func(marker string) {
					t.Helper()
					for {
						select {
						case line, ok := <-fastEvents:
							if !ok {
								t.Fatal("healthy peer closed")
							}
							if strings.Contains(line, marker) {
								return
							}
						case <-time.After(8 * time.Second):
							t.Fatalf("healthy peer missing %s", marker)
						}
					}
				}
				waitFast(`"type":"added"`)
				start := time.Now()
				if trigger == "expiry" {
					expire()
				}
				if trigger == "revocation" {
					revoked.Store(true)
				}
				waitTransport(t, slowDone)
				t.Logf("%s to slow handler return: %s (write budget %s; 8s test tolerance)", trigger, time.Since(start), g.WriteTimeout)
				b.events <- WatchEvent{Type: WatchAdded, Object: obj("later", "still-live", "2")}
				waitFast("still-live")
				if h2 && conns.Load() != 1 {
					t.Fatalf("isolation used %d connections", conns.Load())
				}
				fastClosing.Store(true)
				_ = fast.Body.Close()
				waitTransport(t, fastDone)
				waitTransport(t, b.stopped)
				// Join the reader so it cannot report into a finished subtest.
				waitTransport(t, readerDone)
			})
		}
	}
}

func TestHealthyHTTPStreamOutlivesWriteTimeout(t *testing.T) {
	b := newTransportBackend()
	g := transportGateway(b)
	g.WriteTimeout = 40 * time.Millisecond
	// Idle longer than a write period between heartbeats: stale deadlines would kill it.
	g.HeartbeatInterval = 120 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { g.ServeStream(w, r, nil, sharedScopeUnderTest) }))
	defer srv.Close()
	client := srv.Client()
	client.Timeout = 2 * time.Second
	res, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	reader := bufio.NewReader(res.Body)
	n := 0
	for n < 3 {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, ": heartbeat") {
			n++
		}
	}
}
