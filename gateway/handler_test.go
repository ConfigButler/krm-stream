package gateway_test

import (
	"context"
	"errors"
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
		Clients: func(context.Context, string, gateway.Principal) (gateway.Backend, error) {
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
			code:   "UNAUTHENTICATED",
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

func TestHandlerAuthenticatesBeforeDescribingScopeFailures(t *testing.T) {
	o := testOptions(configmapsAllowed)
	o.Principal = func(*http.Request) (gateway.Principal, error) { return nil, http.ErrNoCookie }
	rec := serve(t, o, "/s?resource=secrets") // malformed and disallowed, but caller is unauthenticated
	if body := rec.Body.String(); !strings.Contains(body, `"code":"UNAUTHENTICATED"`) || strings.Contains(body, `"code":"SCOPE_INVALID"`) {
		t.Errorf("unauthenticated request disclosed scope validation: %s", body)
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
		{"no Principal", gateway.Options{Authorizer: gateway.AllowAll{}, Clients: func(context.Context, string, gateway.Principal) (gateway.Backend, error) { return nil, nil }}},
		{"no Authorizer", gateway.Options{Principal: func(*http.Request) (gateway.Principal, error) { return nil, nil }, Clients: func(context.Context, string, gateway.Principal) (gateway.Backend, error) { return nil, nil }}},
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

// A host chooses what an unidentified caller is told. "Sign in again" is wrong when the host's session
// store is down; "try later" is the honest answer, and it is not terminal.
func TestPrincipalChoosesItsRefusal(t *testing.T) {
	o := testOptions(configmapsAllowed)
	o.Principal = func(*http.Request) (gateway.Principal, error) {
		return nil, gateway.UpstreamUnavailable("session store unavailable", 2*time.Second)
	}
	body := serve(t, o, "/s?version=v1&resource=configmaps&namespace=app").Body.String()
	for _, want := range []string{`"code":"UPSTREAM_UNAVAILABLE"`, `"terminal":false`, `"retryAfterMs":2000`, `session store unavailable`} {
		if !strings.Contains(body, want) {
			t.Errorf("want %s in:\n%s", want, body)
		}
	}
}

func TestPrincipalErrorTextReachesOnlyDiagnostics(t *testing.T) {
	const secret = "redis://session-store.internal:6379"
	var got []gateway.Diagnostic
	o := testOptions(configmapsAllowed)
	o.Principal = func(*http.Request) (gateway.Principal, error) { return nil, errors.New("dial " + secret) }
	o.Diagnostics = func(d gateway.Diagnostic) { got = append(got, d) }

	body := serve(t, o, "/s?version=v1&resource=configmaps&namespace=app").Body.String()
	if strings.Contains(body, secret) {
		t.Fatalf("the host's error text reached the wire:\n%s", body)
	}
	if len(got) != 1 || got[0].Code != gateway.CodeUnauthenticated || !strings.Contains(got[0].Err.Error(), secret) {
		t.Fatalf("diagnostics = %+v, want one UNAUTHENTICATED carrying the raw error", got)
	}
}

// An unexpected backend error is INTERNAL, and its text may name anything: the cluster's internal
// address, a URL a backend promised never to show a browser. The wire says "internal error"; the host
// sees the rest.
func TestUnexpectedErrorTextReachesOnlyDiagnostics(t *testing.T) {
	const internalURL = "https://10.43.0.1:443/api/v1/namespaces/app/configmaps"
	var got []gateway.Diagnostic
	o := testOptions(configmapsAllowed)
	o.Clients = func(context.Context, string, gateway.Principal) (gateway.Backend, error) {
		return failingBackend{errors.New("streaming list for " + internalURL + ": boom")}, nil
	}
	o.Diagnostics = func(d gateway.Diagnostic) { got = append(got, d) }

	body := serve(t, o, "/s?version=v1&resource=configmaps&namespace=app").Body.String()
	if strings.Contains(body, internalURL) || !strings.Contains(body, `"message":"internal error"`) {
		t.Fatalf("want a generic INTERNAL, got:\n%s", body)
	}
	if len(got) != 1 || got[0].Principal != "alice" || got[0].Scope.Resource != "configmaps" || !strings.Contains(got[0].Err.Error(), internalURL) {
		t.Fatalf("diagnostics = %+v, want the raw error with its principal and scope", got)
	}
}

type failingBackend struct{ err error }

func (b failingBackend) Watch(context.Context, gateway.Scope) (gateway.Watcher, error) {
	return nil, b.err
}

// A retryable upstream error is sent with its code and hint, and then the connection closes: the
// client owns the retry. The handler must not resnapshot in a loop against a dead API server.
func TestRetryableUpstreamErrorEndsTheConnection(t *testing.T) {
	attempts := 0
	o := testOptions(configmapsAllowed)
	o.Clients = func(context.Context, string, gateway.Principal) (gateway.Backend, error) {
		attempts++
		return failingBackend{gateway.UpstreamUnavailable("the API server is unavailable", 3*time.Second)}, nil
	}
	body := serve(t, o, "/s?version=v1&resource=configmaps&namespace=app").Body.String()
	if attempts != 1 {
		t.Errorf("upstream attempts = %d, want 1: the gateway retried on its own", attempts)
	}
	want := `data: {"seq":1,"type":"error","code":"UPSTREAM_UNAVAILABLE","message":"the API server is unavailable","retryAfterMs":3000,"terminal":false}`
	if strings.TrimSpace(body) != want {
		t.Errorf("body:\n%s\nwant:\n%s", body, want)
	}
}

// AnyResource is a host policy that delegates resource admission to the upstream's own
// authorization. It admits any resource, with or without a namespace, but keeps the target allowlist.
func TestAnyResourceDelegatesResourceAdmission(t *testing.T) {
	policy := gateway.ScopePolicy{Targets: []string{""}, AnyResource: true}
	for _, target := range []string{
		"/s?group=hello.krm-foyer.example&version=v1&resource=notes&namespace=hello",
		"/s?version=v1&resource=namespaces",
		"/s?version=v1&resource=configmaps",
	} {
		if body := serve(t, testOptions(policy), target).Body.String(); !strings.Contains(body, `"type":"synced"`) {
			t.Errorf("%s was not streamed:\n%s", target, body)
		}
	}
	for _, target := range []string{
		"/s?target=other&version=v1&resource=configmaps&namespace=app",
		"/s?version=v1&resource=configmaps&namespace=app&labelSelector=a%3Db",
		"/s?version=v1&resource=configmaps&server=https://10.0.0.1:6443",
	} {
		if body := serve(t, testOptions(policy), target).Body.String(); !strings.Contains(body, `"code":"SCOPE_INVALID"`) {
			t.Errorf("%s was not refused:\n%s", target, body)
		}
	}
}

func TestAnyResourceWithAnAllowlistIsRefused(t *testing.T) {
	policy := gateway.ScopePolicy{Targets: []string{""}, AnyResource: true, Resources: configmapsAllowed.Resources}
	if err := policy.Validate(gateway.Scope{Version: "v1", Resource: "configmaps", Namespace: "app"}); err == nil {
		t.Error("Validate admitted a scope under an ambiguous policy")
	}
	defer func() {
		if recover() == nil {
			t.Error("Handler mounted an ambiguous scope policy")
		}
	}()
	gateway.Handler(testOptions(policy))
}
