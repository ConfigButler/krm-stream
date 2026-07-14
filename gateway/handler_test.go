package gateway_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ConfigButler/krm-stream/gateway"
)

// The Handler exists because the README promised it and it did not exist — so these tests are about
// the promise: mount it in one line, and the security-relevant glue (scope parsing, the allowlist,
// the principal) is the library's job rather than four chances per adopter to get it subtly wrong.

func testOptions(policy gateway.ScopePolicy) gateway.Options {
	return gateway.Options{
		Principal:  func(*http.Request) (gateway.Principal, error) { return "alice", nil },
		Authorizer: gateway.AllowAll{},
		Clients: func(string, gateway.Principal) (gateway.Backend, error) {
			return &emptyBackend{}, nil
		},
		Scopes: policy,
	}
}

// emptyBackend snapshots nothing, closes the snapshot with the boundary bookmark, and then goes
// quiet: enough to prove the stream opened and framed a cycle.
//
// It goes QUIET rather than returning ErrWatchClosed, and that is not a detail — a closed watch means
// "reopen", so a watcher that closes immediately makes the gateway spin fresh snapshot cycles
// forever. (It did, the first time this file ran: 164 seconds, then the test binary was killed. The
// stream loop was behaving exactly as designed; the stub was the liar.) A real API server idles.
type emptyBackend struct{}

func (*emptyBackend) Watch(context.Context, gateway.Scope) (gateway.Watcher, error) {
	return &emptyWatcher{}, nil
}

type emptyWatcher struct{ sent bool }

func (w *emptyWatcher) Next(ctx context.Context) (gateway.WatchEvent, error) {
	if !w.sent {
		w.sent = true
		return gateway.WatchEvent{Type: gateway.WatchBookmark, InitialEventsEnd: true}, nil
	}
	<-ctx.Done() // a live, quiet upstream: nothing is happening in this namespace
	return gateway.WatchEvent{}, ctx.Err()
}
func (*emptyWatcher) Stop() {}

var configmapsAllowed = gateway.ScopePolicy{
	Targets:   []string{""},
	Resources: []gateway.GroupResource{{Resource: "configmaps", Scope: gateway.ResourceScopeNamespaced}},
}

// serve runs one request to completion. The context is bounded because a HEALTHY stream never ends
// on its own — that is the point of it — so the browser going away is what closes it, and here that
// is the deadline.
func serve(t *testing.T, o gateway.Options, target string) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx)
	gateway.Handler(o).ServeHTTP(rec, req)
	return rec
}

func TestHandlerServesAConformingStream(t *testing.T) {
	rec := serve(t, testOptions(configmapsAllowed), "/s?version=v1&resource=configmaps&namespace=app")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"reset"`) || !strings.Contains(body, `"type":"synced"`) {
		t.Errorf("not a snapshot cycle:\n%s", body)
	}
}

// Every rejection is a TERMINAL SSE error over a 200, and this is the test that pins that choice.
//
// A 403 would be the obvious thing and it is the wrong thing: EventSource cannot read the body of a
// non-200, so the page gets an `onerror` with no code, no message and no reason, and the developer
// is left guessing. `terminal` is also what stops the browser reconnecting to a scope that can never
// become valid.
func TestHandlerRefusalsAreTerminalStreamEvents(t *testing.T) {
	for _, tc := range []struct {
		name, target, code string
		opts               gateway.Options
	}{
		{
			name:   "a malformed scope",
			target: "/s?resource=configmaps", // no version
			code:   "SCOPE_INVALID",
			opts:   testOptions(configmapsAllowed),
		},
		{
			name:   "a resource that is not allowlisted",
			target: "/s?version=v1&resource=secrets&namespace=app",
			code:   "SCOPE_INVALID",
			opts:   testOptions(configmapsAllowed),
		},
		{
			name:   "an API-server address, which is REFUSED and not merely ignored",
			target: "/s?version=v1&resource=configmaps&server=https://10.0.0.1:6443",
			code:   "SCOPE_INVALID",
			opts:   testOptions(configmapsAllowed),
		},
		{
			name:   "a caller we cannot identify",
			target: "/s?version=v1&resource=configmaps&namespace=app",
			code:   "FORBIDDEN",
			opts: func() gateway.Options {
				o := testOptions(configmapsAllowed)
				o.Principal = func(*http.Request) (gateway.Principal, error) {
					return nil, http.ErrNoCookie
				}
				return o
			}(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(t, tc.opts, tc.target)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: EventSource cannot read the body of a non-200, so a "+
					"status code here reaches the page as an onerror with no reason at all", rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, `"code":"`+tc.code+`"`) {
				t.Errorf("want code %s, got:\n%s", tc.code, body)
			}
			if !strings.Contains(body, `"terminal":true`) {
				t.Errorf("a refusal MUST be terminal or EventSource retries it forever:\n%s", body)
			}
			if strings.Contains(body, `"type":"reset"`) {
				t.Error("a refused scope opened a snapshot cycle — the watch should never have been opened")
			}
		})
	}
}

// Deny-by-default, and this is the whole reason ScopePolicy's zero value is what it is: a host that
// forgets to configure the allowlist serves NOTHING, rather than serving Secrets from every
// namespace in every cluster it can reach.
func TestTheZeroScopePolicyStreamsNothing(t *testing.T) {
	rec := serve(t, testOptions(gateway.ScopePolicy{}), "/s?version=v1&resource=configmaps&namespace=app")

	if !strings.Contains(rec.Body.String(), `"code":"SCOPE_INVALID"`) {
		t.Errorf("an unconfigured ScopePolicy served a stream:\n%s", rec.Body.String())
	}
}

// A missing Authorizer is a vulnerability, so it fails at MOUNT time — on the first line of main(),
// not on a request from a real user hours later.
func TestHandlerPanicsOnMissingSeams(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts gateway.Options
	}{
		{"no Principal", gateway.Options{Authorizer: gateway.AllowAll{}, Clients: func(string, gateway.Principal) (gateway.Backend, error) { return nil, nil }}},
		{"no Authorizer", gateway.Options{Principal: func(*http.Request) (gateway.Principal, error) { return nil, nil }, Clients: func(string, gateway.Principal) (gateway.Backend, error) { return nil, nil }}},
		{"no Clients", gateway.Options{Principal: func(*http.Request) (gateway.Principal, error) { return nil, nil }, Authorizer: gateway.AllowAll{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("Handler accepted incomplete Options — a stream gateway that silently " +
						"defaults its authorizer is a vulnerability with a changelog entry")
				}
			}()
			gateway.Handler(tc.opts)
		})
	}
}
