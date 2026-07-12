package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

// These tests do not yet exercise a gateway — there isn't one. They exercise the CONTRACT: that the
// fixtures load, that every object a scenario names exists, that every event is one the protocol
// actually defines, and that the mandatory fields are mandatory.
//
// That is not a placeholder. A fixture corpus that has drifted from the spec is worse than no
// corpus, because both implementations will faithfully agree on the wrong thing. This is the test
// that keeps the contract honest while the two sides are still being built against it.

func corpus(t *testing.T) Corpus {
	t.Helper()
	c, err := LoadConformance()
	if err != nil {
		t.Fatalf("load conformance: %v", err)
	}
	if len(c.Fixtures) == 0 || len(c.Bodies) == 0 {
		t.Fatal("conformance corpus is empty — run `task fixtures`")
	}
	return c
}

func TestFixturesResolve(t *testing.T) {
	c := corpus(t)
	for _, f := range c.Fixtures {
		t.Run(f.ID, func(t *testing.T) {
			if f.Title == "" || f.Why == "" {
				t.Error("every fixture must say what it is and which rule it defends (title + why)")
			}
			for i, fe := range f.Events {
				ev, err := c.Resolve(f.Scope, f.Projection, fe)
				if err != nil {
					t.Fatalf("event %d: %v", i, err)
				}
				if _, err := ev.MarshalSSE(); err != nil {
					t.Fatalf("event %d: marshal: %v", i, err)
				}
			}
			for _, op := range f.Watch {
				refs := op.Bodies
				if op.Body != "" {
					refs = append(refs, op.Body)
				}
				for _, ref := range refs {
					if _, err := c.Body(ref); err != nil {
						t.Errorf("watch op %q: %v", op.Op, err)
					}
				}
			}
		})
	}
}

// Every object on the wire must be identifiable, because the consumer keys on uid and MUST NOT key
// on name. An object without one is a bug that would surface as two objects merging into one.
func TestEveryBodyHasAUID(t *testing.T) {
	c := corpus(t)
	for ref, obj := range c.Bodies {
		if obj.UID() == "" {
			t.Errorf("body %q has no metadata.uid", ref)
		}
	}
}

// The framing rule, checked against the corpus itself: a cycle is reset … added* … synced, and a
// consumer prunes only on synced. A fixture that emits `added` outside a cycle would be teaching
// both implementations a protocol we did not write.
func TestSnapshotFraming(t *testing.T) {
	c := corpus(t)
	for _, f := range c.Fixtures {
		t.Run(f.ID, func(t *testing.T) {
			inCycle := false
			sawReset := false
			endedTerminally := false
			for i, fe := range f.Events {
				if endedTerminally {
					t.Errorf("event %d: %s AFTER a terminal error — a terminal error is the last event (spec §4.3)", i, fe.Type)
				}
				switch fe.Type {
				case EventReset:
					if inCycle {
						t.Errorf("event %d: reset inside an unclosed cycle", i)
					}
					inCycle, sawReset = true, true
				case EventSynced:
					if !inCycle {
						t.Errorf("event %d: synced without a reset", i)
					}
					inCycle = false
				case EventError:
					if !sawReset {
						t.Errorf("event %d: error before the first reset — a consumer has no scope yet", i)
					}
					endedTerminally = fe.Terminal
				case EventAdded, EventModified, EventDeleted:
					if !sawReset {
						t.Errorf("event %d: %s before the first reset — a consumer has no scope yet", i, fe.Type)
					}
				}
			}
			// A stream may legally end mid-cycle, in exactly two ways, and both are fixtures:
			//
			//   - the connection died (partial-cycle-no-prune) — and the consumer must prune NOTHING;
			//   - a TERMINAL error ended it (resourceversion-unorderable) — a terminal error is the
			//     last event on the connection, and the gateway then closes it (spec §4.3). It can
			//     perfectly well arrive mid-snapshot: the gateway only discovers that this upstream is
			//     not what it was promised when the first object shows up.
			//
			// Anything else that ends mid-cycle is a fixture whose author did not notice.
			if inCycle && !endedTerminally && f.ID != "partial-cycle-no-prune" {
				t.Error("stream ends mid-cycle; if that is the point of this fixture, say so in `why`")
			}
		})
	}
}

// redactedPaths is REQUIRED on every added/modified — present, not merely optional — so that a
// consumer never has to infer redaction from a value that happens to look like a placeholder.
func TestRedactedPathsAlwaysPresent(t *testing.T) {
	c := corpus(t)
	for _, f := range c.Fixtures {
		for i, fe := range f.Events {
			if fe.Type != EventAdded && fe.Type != EventModified {
				continue
			}
			ev, err := c.Resolve(f.Scope, f.Projection, fe)
			if err != nil {
				t.Fatalf("%s event %d: %v", f.ID, i, err)
			}
			b, err := json.Marshal(ev)
			if err != nil {
				t.Fatalf("%s event %d: %v", f.ID, i, err)
			}
			if !strings.Contains(string(b), `"redactedPaths"`) {
				t.Errorf("%s event %d: added/modified must carry redactedPaths (empty is fine, absent is not)", f.ID, i)
			}
		}
	}
}

// A deleted event carries an identity with a trustworthy uid — or it is not emitted at all and the
// gateway begins a new snapshot cycle instead. An ambiguous tombstone is worse than a relist.
func TestDeletedCarriesIdentity(t *testing.T) {
	c := corpus(t)
	for _, f := range c.Fixtures {
		for i, fe := range f.Events {
			if fe.Type != EventDeleted {
				continue
			}
			if fe.Identity == nil || fe.Identity.UID == "" || fe.Identity.Name == "" || fe.Identity.Kind == "" {
				t.Errorf("%s event %d: deleted needs a complete identity (uid, apiVersion, kind, name)", f.ID, i)
			}
		}
	}
}

// v1 forbids SSE id: lines. Putting a resource uid there — the tempting thing — would give the
// browser's automatic Last-Event-ID reconnect an entirely incorrect meaning.
func TestSSEFramingEmitsNoIDLine(t *testing.T) {
	c := corpus(t)
	f := c.Fixtures[0]
	ev, err := c.Resolve(f.Scope, f.Projection, f.Events[0])
	if err != nil {
		t.Fatal(err)
	}
	frame, err := ev.MarshalSSE()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(frame), "id:") {
		t.Errorf("v1 emits no SSE id: lines; got %q", frame)
	}
	if !strings.HasSuffix(string(frame), "\n\n") {
		t.Errorf("an SSE frame must end with a blank line; got %q", frame)
	}
}

// Every body must be reachable from some fixture. An orphan body is a scenario someone meant to
// write and didn't — which is exactly the kind of gap this corpus exists to make visible.
func TestNoOrphanBodies(t *testing.T) {
	c := corpus(t)
	used := map[string]bool{}
	for _, f := range c.Fixtures {
		for _, fe := range f.Events {
			if fe.Body != "" {
				used[fe.Body] = true
			}
		}
		for _, op := range f.Watch {
			if op.Body != "" {
				used[op.Body] = true
			}
			for _, b := range op.Bodies {
				used[b] = true
			}
		}
	}
	for ref := range c.Bodies {
		if !used[ref] {
			t.Errorf("body %q is used by no fixture — write the scenario, or delete the body", ref)
		}
	}
}
