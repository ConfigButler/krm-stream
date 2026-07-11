package gateway

import (
	"context"
	"fmt"
	"sync"
)

// ScriptedBackend is a fake Kubernetes watch driven by a fixture's `watch:` ops. It is the gateway's
// half of the conformance corpus, and it is a normal (non-test) file on purpose: the replay server
// serves fixtures over real SSE with exactly this backend, so a browser can be pointed at a scripted
// cluster that behaves identically every time.
//
// It models a MODERN streaming list (gateway spec §3a), because that is what the gateway is written
// against: the objects in scope arrive as synthetic ADDEDs, terminated by a bookmark whose
// InitialEventsEnd is set. That bookmark IS `synced`. A `relist` op ends the watch with a
// continuity-losing error, which is what a 410 Gone looks like from in here — and the gateway must
// then recover on the SAME connection, which is the one thing a fake watch can prove and a real
// cluster can only occasionally be provoked into demonstrating.

// scriptedSegment is everything one Watch() call delivers: a snapshot, then live events, then
// (unless it is the last) the error that ends it.
type scriptedSegment struct {
	events []WatchEvent
}

// ScriptedBackend replays a watch script. Not safe for concurrent Watch calls, and does not need to
// be: one stream, one upstream.
type ScriptedBackend struct {
	segments []scriptedSegment

	mu        sync.Mutex
	segment   int // the next segment Watch() will hand out
	exhausted chan struct{}
	once      sync.Once
}

// NewScriptedBackend compiles a fixture's watch ops into a fake upstream.
//
// A `disconnect` op is NOT handled here: it ends the SSE CONNECTION, not the upstream watch, so it
// is the caller who splits the script on it and runs the gateway again. Conflating "the browser went
// away" with "the API server lost continuity" is precisely the bug resync-midstream defends against,
// and a fake that could not tell them apart would be unable to catch it.
func NewScriptedBackend(c Corpus, ops []WatchOp) (*ScriptedBackend, error) {
	b := &ScriptedBackend{exhausted: make(chan struct{})}
	var cur *scriptedSegment

	body := func(ref string) (KRMObject, error) { return c.Body(ref) }

	for _, op := range ops {
		switch op.Op {
		case "list", "relist":
			if op.Op == "relist" {
				if cur == nil {
					return nil, fmt.Errorf("scripted: `relist` with no watch open")
				}
				// 410 Gone. The connection is fine; our knowledge of the world is not.
				cur.events = append(cur.events, WatchEvent{
					Type: WatchError,
					Err:  ResyncRequired("the upstream resourceVersion expired (410 Gone)"),
				})
			}
			b.segments = append(b.segments, scriptedSegment{})
			cur = &b.segments[len(b.segments)-1]

			for _, ref := range op.Bodies {
				obj, err := body(ref)
				if err != nil {
					return nil, err
				}
				cur.events = append(cur.events, WatchEvent{Type: WatchAdded, Object: obj})
			}
			// The bookmark that closes the snapshot. An EMPTY list still gets one — that is the
			// named-object-absent case, and emitting nothing at all instead is the mistake that
			// leaves a deleted object on screen as a ghost forever.
			cur.events = append(cur.events, WatchEvent{Type: WatchBookmark, InitialEventsEnd: true})

		case "added", "modified", "deleted":
			if cur == nil {
				return nil, fmt.Errorf("scripted: %q before any list", op.Op)
			}
			obj, err := body(op.Body)
			if err != nil {
				return nil, err
			}
			cur.events = append(cur.events, WatchEvent{Type: WatchEventType(op.Op), Object: obj})

		case "disconnect":
			return nil, fmt.Errorf("scripted: `disconnect` is the caller's to handle — split the script on it")

		default:
			return nil, fmt.Errorf("scripted: unknown watch op %q", op.Op)
		}
	}
	if len(b.segments) == 0 {
		return nil, fmt.Errorf("scripted: a watch script must begin with a `list`")
	}
	return b, nil
}

// Exhausted is closed once the gateway has come back for an event after being handed the last one in
// the script — which is proof it finished processing it.
//
// That is the whole reason Watcher is pull-based. With a channel, "the last event was delivered" and
// "the last event was handled" are different moments and a test has to sleep between them; here they
// are the same moment, and the suite is deterministic for free.
func (b *ScriptedBackend) Exhausted() <-chan struct{} { return b.exhausted }

// Watch hands out the next segment of the script.
func (b *ScriptedBackend) Watch(_ context.Context, _ Scope) (Watcher, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.segment >= len(b.segments) {
		return nil, fmt.Errorf("scripted: the gateway reopened the watch %d times; the script has %d segments", b.segment+1, len(b.segments))
	}
	seg := b.segments[b.segment]
	b.segment++
	last := b.segment == len(b.segments)
	return &scriptedWatcher{backend: b, events: seg.events, last: last}, nil
}

type scriptedWatcher struct {
	backend *ScriptedBackend
	events  []WatchEvent
	last    bool
	i       int
}

func (w *scriptedWatcher) Next(ctx context.Context) (WatchEvent, error) {
	if w.i < len(w.events) {
		ev := w.events[w.i]
		w.i++
		return ev, nil
	}
	if !w.last {
		// Only the final segment runs dry: every other one ends with the error that made the gateway
		// reopen. Reaching here otherwise means the script and the gateway disagree about how many
		// watches this scenario opens, which is worth saying out loud rather than hanging.
		return WatchEvent{}, fmt.Errorf("scripted: segment ended without a continuity error")
	}
	w.backend.once.Do(func() { close(w.backend.exhausted) })

	// The script is over, and a real watch would simply be quiet here. Blocking (rather than
	// closing) is the honest simulation: a closed watch means "reopen with a fresh snapshot", and
	// synthesizing that at the end of every fixture would append a phantom cycle to the expected
	// events of every single scenario.
	<-ctx.Done()
	return WatchEvent{}, ctx.Err()
}

func (w *scriptedWatcher) Stop() {}
