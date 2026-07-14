package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// The gateway's half of the corpus, actually asserted.
//
// conformance_test.go checks that the corpus is well-formed. This checks that the GATEWAY is: each
// fixture's `watch:` ops are played through a fake Kubernetes watch, and what the gateway puts on
// the wire is compared to the fixture's `events:` — byte for byte, because `events:` is the contract
// the TypeScript store on the other side is fed.
//
// No cluster. That is not a compromise: every rule in this corpus is about FRAMING (reset…synced,
// pruning, tombstones, resync) and PROJECTION, and a fake watch drives those deterministically in
// microseconds. What a fake cannot prove — that a real API server's streaming list really does end
// its snapshot with an `initial-events-end` bookmark, that a real 410 arrives as we expect — is the
// job of the k3d suite, and of nothing below it.

func TestGatewayConformance(t *testing.T) {
	c := corpus(t)
	ran := 0
	for _, f := range c.Fixtures {
		if !f.Suite("gateway") || len(f.Watch) == 0 {
			continue
		}
		ran++
		t.Run(f.ID, func(t *testing.T) {
			t.Logf("defends: %s", f.Why)

			want := make([]Event, 0, len(f.Events))
			for i, fe := range f.Events {
				ev, err := c.Resolve(f.Scope, f.Projection, fe)
				if err != nil {
					t.Fatalf("fixture event %d: %v", i, err)
				}
				want = append(want, ev)
			}

			got := replayFixture(t, c, f)
			assertEventsEqual(t, want, got)
		})
	}
	if ran == 0 {
		t.Fatal("no gateway fixtures ran — the suite is asserting nothing at all")
	}
}

// replayFixture drives one fixture's `watch:` script through the gateway and returns everything the
// gateway emitted, across every connection the script implies.
//
// A `disconnect` op ends the SSE CONNECTION: the browser's EventSource dropped, and what follows is
// a whole new stream (a new Run, a new reset). A `relist` does NOT — upstream continuity was lost
// while the connection stayed perfectly healthy, and the gateway must recover ON the live connection
// (protocol §5). Conflating those two is the bug `resync-midstream` exists to catch, so the harness
// has to keep them apart or the fixture proves nothing.
func replayFixture(t *testing.T, c Corpus, f Fixture) []Event {
	t.Helper()
	var got []Event
	replay(t, c, f, func(int) Sink { return &recordingSink{} }, func(s Sink) {
		got = append(got, s.(*recordingSink).events...)
	})
	return got
}

// replay is the driver both the event assertion and the SSE goldens share. Sharing it is the point:
// the golden transcripts are the bytes THIS gateway wrote, through its real SSE sink, rather than a
// second encoding of the same events written for the benefit of the test.
func replay(t *testing.T, c Corpus, f Fixture, newSink func(conn int) Sink, done func(Sink)) {
	t.Helper()

	gw := &Gateway{Auth: AllowAll{}, Projection: f.Projection}

	connections := splitConnections(f.Watch)
	endsTerminally := len(f.Events) > 0 && f.Events[len(f.Events)-1].Type == EventError && f.Events[len(f.Events)-1].Terminal
	for i, conn := range connections {
		backend, err := NewScriptedBackend(c, conn)
		if err != nil {
			t.Fatalf("scripted backend: %v", err)
		}
		gw.Clients = func(context.Context, string, Principal) (Backend, error) { return backend, nil }

		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		sink := newSink(i)

		finished := make(chan error, 1)
		go func() { finished <- gw.Stream(ctx, nil, *f.Scope, sink) }()

		// The script is exhausted the moment the gateway comes BACK for another event having been
		// given the last one — which is proof it finished processing it. No sleeps, no polling: a
		// pull-based upstream makes the handoff a synchronisation point for free.
		select {
		case <-backend.Exhausted():
			if endsTerminally && i == len(connections)-1 {
				t.Fatal("gateway requested another upstream event after emitting a terminal error")
			}
			cancel()
			if err := <-finished; err != nil && !isCanceled(err) {
				t.Fatalf("stream: %v", err)
			}
		case err := <-finished:
			// The gateway stopped early. That is legal in exactly one way: a TERMINAL error, which is
			// the last event on the connection, after which the gateway closes it (spec §4.3). The
			// gateway is entitled to decide mid-script that this upstream is not one it can serve —
			// see resourceversion-unorderable — and refusing loudly is the whole point of that fixture.
			var se *StreamError
			if !errors.As(err, &se) || !se.Terminal {
				t.Fatalf("the gateway stopped before the script did, and not with a terminal error: %v", err)
			}
			cancel()
		case <-ctx.Done():
			t.Fatal("the gateway never consumed the whole watch script")
		}
		done(sink)
	}
}

// splitConnections cuts the watch script at each `disconnect`.
func splitConnections(ops []WatchOp) [][]WatchOp {
	conns := [][]WatchOp{{}}
	for _, op := range ops {
		if op.Op == "disconnect" {
			conns = append(conns, []WatchOp{})
			continue
		}
		conns[len(conns)-1] = append(conns[len(conns)-1], op)
	}
	out := conns[:0]
	for _, c := range conns {
		if len(c) > 0 {
			out = append(out, c)
		}
	}
	return out
}

func assertEventsEqual(t *testing.T, want, got []Event) {
	t.Helper()
	for i := range max(len(want), len(got)) {
		switch {
		case i >= len(got):
			t.Errorf("event %d: MISSING\n  want: %s", i, mustJSON(t, want[i]))
		case i >= len(want):
			t.Errorf("event %d: UNEXPECTED\n  got:  %s", i, mustJSON(t, got[i]))
		default:
			w, g := mustJSON(t, wireForm(want[i])), mustJSON(t, wireForm(got[i]))
			if w != g {
				t.Errorf("event %d differs\n  want: %s\n  got:  %s", i, w, g)
			}
			if got[i].Type == EventError && got[i].Message == "" {
				t.Errorf("event %d: an error with no message is a diagnostic that says nothing", i)
			}
		}
	}
}

// wireForm is the event as the corpus specifies it — which is everything EXCEPT an error's `message`.
//
// This is the one field compared loosely, and not for convenience. `code` is normative: it is a
// stable, machine-readable set precisely because "a free-form message alone is not enough for stable
// client behavior" (spec §4.3), and the corpus's fixture-event type carries no `message` field at
// all, in either loader. Pinning the prose would be asserting a rule the contract does not make —
// and would push a gateway towards emitting nothing, which is worse for whoever has to debug it at
// 3am. So: the code and the terminal flag are pinned exactly, the prose must merely exist.
func wireForm(ev Event) Event {
	ev.Seq = 0
	if ev.Type == EventError {
		ev.Message = ""
	}
	return ev
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func isCanceled(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded
}

// recordingSink is the wire, minus the wire: it keeps what the gateway emitted so the test can
// compare it to the fixture. The SSE sink is the same interface with bytes on the end of it.
type recordingSink struct{ events []Event }

func (s *recordingSink) Emit(_ context.Context, ev Event) error {
	s.events = append(s.events, ev)
	return nil
}
