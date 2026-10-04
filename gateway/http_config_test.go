package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Timed reauthorization over library-owned HTTP needs a write bound. A timed check waits for the
// subscriber's write in progress, and a write to a reader that stopped reading never ends without a
// deadline — so a host that set an interval alone had an interval that did not hold. That combination
// is now refused as configuration, at construction and before any direct serving touches a response.

// spyWriter records every response operation, so a test can prove none happened.
type spyWriter struct {
	header http.Header
	ops    []string
}

func (w *spyWriter) Header() http.Header {
	w.ops = append(w.ops, "Header")
	return w.header
}
func (w *spyWriter) WriteHeader(int) { w.ops = append(w.ops, "WriteHeader") }
func (w *spyWriter) Write(p []byte) (int, error) {
	w.ops = append(w.ops, "Write")
	return len(p), nil
}
func (w *spyWriter) FlushError() error { w.ops = append(w.ops, "Flush"); return nil }
func (w *spyWriter) SetWriteDeadline(time.Time) error {
	w.ops = append(w.ops, "SetWriteDeadline")
	return nil
}

func configPanic(t *testing.T, f func()) (msg string) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("the configuration was accepted")
		}
		s, ok := r.(string)
		if !ok {
			t.Fatalf("panic = %v (%T), want a configuration message", r, r)
		}
		msg = s
	}()
	f()
	return ""
}

func validOptions() Options {
	return Options{
		Principal: func(*http.Request) (Principal, error) { return nil, nil },
		Scopes:    ScopePolicy{Targets: []string{""}, AnyResource: true},
		StreamConfig: StreamConfig{
			Authorizer: AllowAll{},
			Clients:    func(context.Context, string, Principal) (Backend, error) { return nil, nil },
		},
	}
}

func TestHandlerRefusesTimedReauthorizationWithoutAWriteTimeout(t *testing.T) {
	o := validOptions()
	o.ReauthorizationInterval = 30 * time.Second
	msg := configPanic(t, func() { Handler(o) })
	if !strings.Contains(msg, "ReauthorizationInterval requires a positive WriteTimeout") {
		t.Errorf("panic = %q, want it to name both options", msg)
	}

	// The negative-timeout refusal is unchanged, and wins: it is wrong on its own.
	o.WriteTimeout = -time.Second
	if msg := configPanic(t, func() { Handler(o) }); !strings.Contains(msg, "must not be negative") {
		t.Errorf("panic = %q, want the negative-timeout refusal", msg)
	}
}

func TestHandlerAcceptsBoundedTimedReauthorizationAndUntimedStreams(t *testing.T) {
	for name, o := range map[string]func(*Options){
		"timed checks with a write timeout":   func(o *Options) { o.ReauthorizationInterval, o.WriteTimeout = 30*time.Second, 5*time.Second },
		"no timed checks, no write timeout":   func(*Options) {},
		"no timed checks, with write timeout": func(o *Options) { o.WriteTimeout = 5 * time.Second },
	} {
		t.Run(name, func(t *testing.T) {
			opts := validOptions()
			o(&opts)
			if Handler(opts) == nil {
				t.Fatal("no handler")
			}
		})
	}
}

// A host serving directly meets the same rule, and meets it before anything happens: no header, no
// write, no deadline, no stream-opening observation, no authorization, no backend.
func TestDirectHTTPServingRefusesTimedReauthorizationBeforeAnyIO(t *testing.T) {
	for name, serve := range map[string]func(*Gateway, http.ResponseWriter, *http.Request){
		"default projection": func(g *Gateway, w http.ResponseWriter, r *http.Request) {
			g.ServeStream(w, r, "alice", sharedScopeUnderTest, "")
		},
		"requested projection": func(g *Gateway, w http.ResponseWriter, r *http.Request) {
			g.ServeStream(w, r, "alice", sharedScopeUnderTest, ProjectionSpec)
		},
	} {
		t.Run(name, func(t *testing.T) {
			var observed, calls atomic.Int32
			g := &Gateway{
				StreamConfig: StreamConfig{
					Authorizer: AuthorizerFunc(func(context.Context, Principal, Scope) error {
						calls.Add(1)
						return nil
					}),
					Clients: func(context.Context, string, Principal) (Backend, error) {
						calls.Add(1)
						return newFakeUpstream(), nil
					},
					Observer:                ObserverFunc(func(Observation) { observed.Add(1) }),
					ReauthorizationInterval: 30 * time.Second,
				},
			}
			w := &spyWriter{header: http.Header{}}
			msg := configPanic(t, func() { serve(g, w, httptest.NewRequest(http.MethodGet, "/", nil)) })
			if !strings.Contains(msg, "ReauthorizationInterval requires a positive WriteTimeout") {
				t.Errorf("panic = %q", msg)
			}
			if len(w.ops) != 0 {
				t.Errorf("the response was touched before the refusal: %v", w.ops)
			}
			if n := observed.Load(); n != 0 {
				t.Errorf("%d observations before the refusal, want none", n)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("%d host callbacks before the refusal, want none", n)
			}
		})
	}
}

// The same direct paths still serve a stream once the write is bounded, and with timed checks off a
// zero write timeout remains valid.
func TestDirectHTTPServingAcceptsValidTimedConfigurations(t *testing.T) {
	for name, tc := range map[string]struct{ interval, write time.Duration }{
		"timed checks with a write timeout": {time.Hour, time.Second},
		"no timed checks, no write timeout": {0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			g := &Gateway{
				StreamConfig: StreamConfig{
					Authorizer:              AuthorizerFunc(func(context.Context, Principal, Scope) error { return Forbidden("not today") }),
					Clients:                 func(context.Context, string, Principal) (Backend, error) { return newFakeUpstream(), nil },
					WriteTimeout:            tc.write,
					ReauthorizationInterval: tc.interval,
				},
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				g.ServeStream(w, r, "alice", sharedScopeUnderTest, "")
			}))
			defer srv.Close()
			res, err := srv.Client().Get(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = res.Body.Close() }()
			body, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), `"code":"FORBIDDEN"`) {
				t.Errorf("the stream was not served:\n%s", body)
			}
		})
	}
}

// Transport-neutral streams keep timed checks without any HTTP option: their sink is the host's to
// bound. A revocation still ends the stream through it.
func TestTransportNeutralStreamsKeepTimedChecksWithoutAWriteTimeout(t *testing.T) {
	var revoked atomic.Bool
	g := &Gateway{
		StreamConfig: StreamConfig{
			Authorizer: AuthorizerFunc(func(context.Context, Principal, Scope) error {
				if revoked.Load() {
					return Forbidden("revoked")
				}
				return nil
			}),
			Clients:                 func(context.Context, string, Principal) (Backend, error) { return newFakeUpstream(), nil },
			ReauthorizationInterval: time.Millisecond,
		},
	}
	events := make(reauthEvents, 32)
	done := make(chan error, 1)
	go func() { done <- g.Stream(t.Context(), "alice", sharedScopeUnderTest, "", events) }()
	awaitEvent(t, events, EventReset)
	revoked.Store(true)
	if ev := awaitEvent(t, events, EventError); ev.Code != CodeForbidden || !ev.Terminal {
		t.Fatalf("revocation = %+v, want a terminal FORBIDDEN", ev)
	}
	<-done
}
