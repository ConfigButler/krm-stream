package gateway

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
)

// Authorization and credentials, across the LIFE of a stream — not merely at its start.
//
// A stream lives as long as a dashboard tab: hours. An OIDC access token lives 5–60 minutes. Asking
// "may you?" once, at open, and then running forever on the answer is the bug these tests exist to
// prevent — and it was the real behaviour until a reviewer went looking. See docs/auth.md.

// closingBackend ends each watch after the snapshot, which forces a new snapshot cycle. That is the
// checkpoint where entitlement is re-examined, so it is the thing we need to be able to provoke.
type closingBackend struct{ opened atomic.Int32 }

func (b *closingBackend) Watch(context.Context, Scope) (Watcher, error) {
	b.opened.Add(1)
	return &closingWatcher{}, nil
}

type closingWatcher struct{ sent bool }

func (w *closingWatcher) Next(context.Context) (WatchEvent, error) {
	if !w.sent {
		w.sent = true
		return WatchEvent{Type: WatchBookmark, InitialEventsEnd: true}, nil
	}
	return WatchEvent{}, ErrWatchClosed // a routine timeout: the gateway reopens with a fresh cycle
}
func (*closingWatcher) Stop() {}

// authSink collects the wire, and cancels the stream once a terminal error lands.
type authSink struct{ events []Event }

func (s *authSink) Emit(_ context.Context, ev Event) error {
	s.events = append(s.events, ev)
	// Stop the (otherwise endless) stream once we have seen a terminal error.
	if ev.Type == EventError && ev.Terminal {
		return context.Canceled
	}
	return nil
}

func (s *authSink) codes() string {
	var b strings.Builder
	for _, ev := range s.events {
		b.WriteString(string(ev.Type))
		if ev.Code != "" {
			b.WriteString("(" + string(ev.Code) + ")")
		}
		b.WriteString(" ")
	}
	return b.String()
}

// Revoke a user's access and their OPEN stream must stop. It used to keep streaming: the Authorizer
// was consulted once, at open, and never again.
func TestRevokedAccessTerminatesAnOpenStream(t *testing.T) {
	var asked atomic.Int32

	sink := &authSink{}
	g := &Gateway{
		// Entitlement changes UNDER the stream: the first cycle is authorized, and by the time the
		// gateway asks again — which, before this was fixed, it never did — the answer has flipped.
		Auth: AuthorizerFunc(func(context.Context, Principal, Scope) error {
			if asked.Add(1) == 1 {
				return nil
			}
			return Forbidden("your access to this scope was revoked")
		}),
		Clients: func(string, Principal) (Backend, error) { return &closingBackend{}, nil },
	}

	_ = g.Stream(t.Context(), "alice", Scope{Version: "v1", Resource: "configmaps"}, sink)

	if asked.Load() < 2 {
		t.Fatal("the Authorizer was consulted once and never again: a revoked user's open stream " +
			"would keep delivering objects it is no longer entitled to")
	}

	last := sink.events[len(sink.events)-1]
	if last.Type != EventError || last.Code != CodeForbidden {
		t.Fatalf("a revoked user's stream did not end with FORBIDDEN. Got: %s", sink.codes())
	}
	if !last.Terminal {
		t.Error("the refusal must be TERMINAL: EventSource reconnects on its own, so a revoked user " +
			"would otherwise hammer a forbidden scope forever")
	}
}

// ClientFor is re-invoked per cycle, which is the seam a host hangs credential refresh on: it may
// hand back a client bearing a FRESH token each time, so a long stream never runs on a dead one.
func TestTheClientIsResolvedOnEveryCycle(t *testing.T) {
	var resolved atomic.Int32
	backend := &closingBackend{}

	sink := &authSink{}
	g := &Gateway{
		Auth: AllowAll{},
		Clients: func(string, Principal) (Backend, error) {
			// Three cycles is enough to show it is per-cycle and not once-ever.
			if resolved.Add(1) >= 3 {
				return nil, Forbidden("the credential could not be refreshed")
			}
			return backend, nil
		},
	}

	_ = g.Stream(t.Context(), "alice", Scope{Version: "v1", Resource: "configmaps"}, sink)

	if got := resolved.Load(); got < 3 {
		t.Errorf("ClientFor was called %d times across several cycles — a host has nowhere to refresh "+
			"a credential, and a long stream will run on a dead token", got)
	}
	// …and when refresh fails, the consumer is TOLD, rather than being left on a stream that has
	// quietly stopped being authorized.
	last := sink.events[len(sink.events)-1]
	if last.Type != EventError || !last.Terminal {
		t.Errorf("a failed credential refresh did not terminate the stream. Got: %s", sink.codes())
	}
}

// Denial still comes BEFORE any watch is opened. Opening the watch and filtering afterwards has
// already leaked the object's existence — and, if it logs, its contents.
func TestDenialOpensNoWatchAtAll(t *testing.T) {
	backend := &closingBackend{}
	sink := &authSink{}
	g := &Gateway{
		Auth:    AuthorizerFunc(func(context.Context, Principal, Scope) error { return Forbidden("no") }),
		Clients: func(string, Principal) (Backend, error) { return backend, nil },
	}

	_ = g.Stream(t.Context(), "mallory", Scope{Version: "v1", Resource: "secrets"}, sink)

	if backend.opened.Load() != 0 {
		t.Error("a watch was opened for a caller who was refused — the object's existence has leaked")
	}
	if len(sink.events) != 1 || sink.events[0].Code != CodeForbidden {
		t.Errorf("want a single terminal FORBIDDEN, got: %s", sink.codes())
	}
}
