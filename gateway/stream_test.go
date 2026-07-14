package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// The gateway rules the CORPUS cannot express — and the reason it cannot is structural, not an
// oversight: a fixture's `watch:` script has ops for list/added/modified/deleted/relist/disconnect,
// and there is no way to write down "the API server sent a bare BOOKMARK" or "the informer handed us
// a tombstone with no uid". So a fixture cannot drive them, and I proved it: mutating the gateway to
// emit `synced` on every bookmark, or to forward an object with no uid, leaves all 11 gateway
// fixtures green.
//
// They are MUST NOTs (spec §2, §4.2) and rows 7, 8, 14 of the gateway's own failure matrix, so they
// get tested here. If we later teach the fixture format those three ops, these move into the corpus
// where they belong and this file shrinks.

// stubBackend replays a fixed list of upstream events on the FIRST watch, then goes quiet. Unlike
// ScriptedBackend it is not driven by a fixture — it exists to say things a fixture has no
// vocabulary for.
//
// Quiet on reopen, and that is not laziness: several of these scenarios end in a resnapshot, and a
// stub that replayed the same broken tombstone into every new cycle would spin forever. A real
// cluster re-lists into a healthy world; the recovery is the thing under test, not the fault.
type stubBackend struct {
	events  []WatchEvent
	watches int
}

func (b *stubBackend) Watch(_ context.Context, _ Scope) (Watcher, error) {
	b.watches++
	if b.watches > 1 {
		return &stubWatcher{}, nil
	}
	return &stubWatcher{events: b.events}, nil
}

type stubWatcher struct {
	events []WatchEvent
	i      int
}

func (w *stubWatcher) Next(ctx context.Context) (WatchEvent, error) {
	if w.i < len(w.events) {
		ev := w.events[w.i]
		w.i++
		return ev, nil
	}
	<-ctx.Done()
	return WatchEvent{}, ctx.Err()
}

func (w *stubWatcher) Stop() {}

// run drives the gateway over a stub upstream until it has emitted `want` events, and returns them.
//
// The sink stops the stream by refusing the (want+1)th event — a sink error unwinds the loop the
// same way a closed browser connection does. That, rather than a cancel-and-hope, is what makes
// reading `events` afterwards free of a data race.
func run(t *testing.T, projection Projection, auth Authorizer, events []WatchEvent, want int) []Event {
	t.Helper()
	gw := &Gateway{
		Auth:       auth,
		Projection: projection,
		Clients: func(string, Principal) (Backend, error) {
			return &stubBackend{events: events}, nil
		},
	}
	// A deadline, not a bare cancel. A gateway that emits FEWER events than the scenario expects
	// must FAIL — loudly, in seconds — and not hang the suite until CI's own timeout kills it with
	// no useful message. (This is not hypothetical: mutating the partial-object guard away is
	// exactly the bug that produces "four events, then silence".)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	sink := &countingSink{want: want, done: make(chan struct{})}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = gw.Stream(ctx, nil, Scope{Target: "demo", Version: "v1", Resource: "configmaps"}, sink)
	}()

	select {
	case <-sink.done:
	case <-ctx.Done():
		<-stopped
		t.Fatalf("the gateway emitted %d events and then went quiet; the scenario wants %d: %v",
			len(sink.events), want, types(sink.events))
	}
	cancel()
	<-stopped
	return sink.events
}

var errEnough = errors.New("test: that is all the events we wanted")

type countingSink struct {
	want   int
	events []Event
	done   chan struct{}
}

func (s *countingSink) Emit(_ context.Context, ev Event) error {
	if len(s.events) >= s.want {
		return errEnough
	}
	s.events = append(s.events, ev)
	if len(s.events) == s.want {
		close(s.done)
	}
	return nil
}

func types(evs []Event) []EventType {
	out := make([]EventType, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

func equalTypes(a []EventType, b ...EventType) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func cm(uid, rv string, data map[string]any) KRMObject {
	return KRMObject{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"uid": uid, "name": "c", "namespace": "app", "resourceVersion": rv},
		"data":       data,
	}
}

// Spec §2: a BOOKMARK is never forwarded. Only the one that closes the initial events IS the
// snapshot boundary; the rest exist to carry a resourceVersion the browser has no business seeing.
// Emit `synced` for every bookmark and a consumer prunes mid-cycle — which is the one thing pruning
// is gated on `synced` to prevent.
func TestBookmarksAreAbsorbedExceptTheSnapshotBoundary(t *testing.T) {
	got := run(t, ProjectionFull, AllowAll{}, []WatchEvent{
		{Type: WatchAdded, Object: cm("u1", "1", map[string]any{"v": "one"})},
		{Type: WatchBookmark},                         // a routine bookmark, mid-snapshot
		{Type: WatchBookmark, InitialEventsEnd: true}, // THE boundary
		{Type: WatchBookmark},                         // a routine bookmark, live
		{Type: WatchModified, Object: cm("u1", "2", map[string]any{"v": "two"})},
	}, 4)

	if !equalTypes(types(got), EventReset, EventAdded, EventSynced, EventModified) {
		t.Errorf("a bookmark reached the wire: %v", types(got))
	}
}

// Spec §4.2: if the gateway cannot recover a trustworthy uid it MUST NOT emit an ambiguous
// `deleted`. It begins a new snapshot cycle instead and lets reset…synced prune. Guessing here
// deletes the wrong object out of somebody's browser.
func TestDegenerateTombstoneResnapshotsRatherThanGuessing(t *testing.T) {
	got := run(t, ProjectionFull, AllowAll{}, []WatchEvent{
		{Type: WatchAdded, Object: cm("u1", "1", nil)},
		{Type: WatchBookmark, InitialEventsEnd: true},
		// An informer tombstone that lost the object: no uid, no name.
		{Type: WatchDeleted, Object: KRMObject{"kind": "ConfigMap", "apiVersion": "v1", "metadata": map[string]any{}}},
	}, 5)

	if !equalTypes(types(got), EventReset, EventAdded, EventSynced, EventError, EventReset) {
		t.Fatalf("want reset,added,synced,error,reset; got %v", types(got))
	}
	if got[3].Code != CodeResyncRequired || got[3].Terminal {
		t.Errorf("the recovery must be a NON-terminal RESYNC_REQUIRED; got %+v", got[3])
	}
	for _, ev := range got {
		if ev.Type == EventDeleted {
			t.Error("an ambiguous `deleted` reached the wire — that deletes the wrong object")
		}
	}
}

// Spec §2: a metadata-only / partial object is never forwarded as added/modified. The consumer's
// model is REPLACE, so a fragment would blank its state for that uid.
func TestPartialObjectIsNeverForwarded(t *testing.T) {
	got := run(t, ProjectionFull, AllowAll{}, []WatchEvent{
		{Type: WatchAdded, Object: cm("u1", "1", nil)},
		{Type: WatchBookmark, InitialEventsEnd: true},
		{Type: WatchModified, Object: KRMObject{"kind": "ConfigMap", "metadata": map[string]any{"name": "c"}}}, // no uid
	}, 5)

	if !equalTypes(types(got), EventReset, EventAdded, EventSynced, EventError, EventReset) {
		t.Fatalf("a partial object must trigger a resnapshot, not an upsert; got %v", types(got))
	}
}

// Spec §6: within one cycle, never emit a state for a uid older than one already emitted. This is
// what makes coalescing safe, and an informer that relists WILL replay an older version.
func TestStaleResourceVersionIsDropped(t *testing.T) {
	got := run(t, ProjectionFull, AllowAll{}, []WatchEvent{
		{Type: WatchAdded, Object: cm("u1", "10", map[string]any{"v": "a"})},
		{Type: WatchBookmark, InitialEventsEnd: true},
		{Type: WatchModified, Object: cm("u1", "7", map[string]any{"v": "STALE"})}, // older: must not be emitted
		{Type: WatchModified, Object: cm("u1", "11", map[string]any{"v": "b"})},
	}, 4)

	if !equalTypes(types(got), EventReset, EventAdded, EventSynced, EventModified) {
		t.Fatalf("want the stale event dropped; got %v", types(got))
	}
	if data, _ := got[3].Object["data"].(map[string]any); data["v"] != "b" {
		t.Errorf("the consumer was handed a state it had already moved past: %v", data)
	}
}

// resourceVersion comparison, against the rules Kubernetes actually publishes. The corpus covers the
// big-number case (resourceversion-bignum); this covers the cases a fixture cannot reach, because a
// fixture body is a real KRM object and these are not.
func TestCompareResourceVersion(t *testing.T) {
	// The docs' own worked examples, verbatim.
	big40 := "2345678901234567890123456789012345678901"
	big39 := "345678901234567890123456789012345678901"

	cases := []struct {
		a, b string
		want int
		ok   bool
		why  string
	}{
		{big40, big39, 1, true, "40 digits beats 39 — and neither fits in an int64"},
		{big39, big39, 0, true, "equal"},
		{"345678901234567890123456789012345678900", big39, -1, true, "same length: lexicographic"},
		{"123", "23", 1, true, "longer is greater — NOT plain lexicographic, which says '1' < '2'"},
		{"9", "10", -1, true, "the case a naive string compare gets backwards"},
		{"1001", "1002", -1, true, "the ordinary case"},

		// Not orderable. On Kubernetes 1.35+ these cannot occur — orderability is a conformance
		// requirement, for built-ins AND custom resources — so `ok:false` is a statement about
		// non-conformant or aggregated upstreams, not about ordinary ones.
		{"abc", "def", 0, false, "non-decimal: not orderable"},
		{"1001", "abc", 0, false, "one non-decimal: not orderable"},
		{"", "1001", 0, false, "empty: not orderable"},
		{"0123", "1001", 0, false, "a leading zero is not a valid orderable rv (must start 1-9)"},
		{"abc", "abc", 0, false, "equal — but still not ORDERABLE, and the caller must know that"},
	}
	for _, c := range cases {
		got, ok := compareResourceVersion(c.a, c.b)
		if got != c.want || ok != c.ok {
			t.Errorf("compare(%q, %q) = (%d, %v), want (%d, %v) — %s", c.a, c.b, got, ok, c.want, c.ok, c.why)
		}
	}
}

// OrderingStrict is the default because Kubernetes 1.35 made orderability a CONFORMANCE requirement.
// So an unorderable resourceVersion is not a case to tolerate quietly — it means the upstream is not
// what we were told it is, and a consumer that was promised per-object monotonicity is silently no
// longer getting it. Say so, and stop.
func TestStrictOrderingRefusesAnUnorderableResourceVersion(t *testing.T) {
	got := run(t, ProjectionFull, AllowAll{}, []WatchEvent{
		{Type: WatchAdded, Object: cm("u1", "opaque-1", nil)},
	}, 2)

	if !equalTypes(types(got), EventReset, EventError) {
		t.Fatalf("want reset then a terminal error; got %v", types(got))
	}
	if !got[1].Terminal || got[1].Code != CodeInternal {
		t.Errorf("want a terminal INTERNAL; got %+v", got[1])
	}
	// The error must name the way out, or the operator of a pre-1.35 (or aggregated) cluster is simply
	// stuck, with a library that refuses to work and does not say why.
	if !strings.Contains(got[1].Message, "OrderingLenient") {
		t.Errorf("the refusal must name its own escape hatch; got %q", got[1].Message)
	}
}

// OrderingLenient is the escape hatch: a cluster older than 1.35, or an aggregated API server, which
// is a third-party implementation that the conformance test does not cover. There, ordering is
// genuinely undefined — so we do not pretend to it, and we DROP NOTHING. A duplicate is harmless
// (apply is idempotent by construction); a wrongly-dropped update is data loss, and in a status view
// it looks exactly like the cluster being slow.
func TestLenientOrderingDropsNothingItCannotOrder(t *testing.T) {
	gw := &Gateway{
		Auth:     AllowAll{},
		Ordering: OrderingLenient,
		Clients: func(string, Principal) (Backend, error) {
			return &stubBackend{events: []WatchEvent{
				{Type: WatchAdded, Object: cm("u1", "opaque-b", map[string]any{"v": "b"})},
				{Type: WatchBookmark, InitialEventsEnd: true},
				{Type: WatchModified, Object: cm("u1", "opaque-a", map[string]any{"v": "a"})}, // "older"? unknowable
				{Type: WatchModified, Object: cm("u1", "opaque-c", map[string]any{"v": "c"})},
			}}, nil
		},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	sink := &countingSink{want: 4, done: make(chan struct{})}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = gw.Stream(ctx, nil, Scope{Target: "demo"}, sink)
	}()
	select {
	case <-sink.done:
	case <-ctx.Done():
		<-stopped
		t.Fatalf("an unorderable resourceVersion dropped an event: got %v", types(sink.events))
	}
	cancel()
	<-stopped

	if !equalTypes(types(sink.events), EventReset, EventAdded, EventSynced, EventModified) {
		t.Fatalf("want the unorderable update let through; got %v", types(sink.events))
	}
}

// (The other half — that ordering, when we HAVE it, really does drop a stale replay — is
// TestStaleResourceVersionIsDropped above. Strict mode keeps the protocol's promise; lenient mode
// gives it up only where Kubernetes itself does.)

// The projection removes machinery — and ONLY what a named projection says it removes. Nothing is
// "optionally other server-side bookkeeping".
func TestProjectionRemovesMachineryAndNothingElse(t *testing.T) {
	obj := KRMObject{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"uid": "u1", "name": "c", "resourceVersion": "1",
			"managedFields": []any{map[string]any{"manager": "kubectl"}},
			"annotations":   map[string]any{lastAppliedAnnotation: "{...}", "note": "keep"},
		},
		"data": map[string]any{"a": "1"},
	}
	out, paths := project(ProjectionFull, obj)
	meta := out["metadata"].(map[string]any)

	if _, ok := meta["managedFields"]; ok {
		t.Error("managedFields must never reach a human editor — nor round-trip back")
	}
	ann := meta["annotations"].(map[string]any)
	if _, ok := ann[lastAppliedAnnotation]; ok {
		t.Error("last-applied-configuration must be removed")
	}
	if ann["note"] != "keep" {
		t.Error("the projection removed something it does not declare")
	}
	if len(paths) != 0 {
		t.Errorf("removal is not redaction: redacted must be empty here, got %v", paths)
	}
	// Removal must not mutate the caller's object: an informer hands the same pointer to every
	// subscriber, so an in-place edit here corrupts the object for every other browser on this scope.
	if _, ok := obj["metadata"].(map[string]any)["managedFields"]; !ok {
		t.Error("project() mutated the upstream's object in place")
	}
}

// An annotations map that is empty ONLY because the projection emptied it is our artifact, not the
// server's state — and "has an empty annotation map" is a different fact from "has none".
func TestProjectionDoesNotLeaveAnEmptyAnnotationsMap(t *testing.T) {
	out, _ := project(ProjectionFull, KRMObject{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"uid": "u1", "name": "c", "annotations": map[string]any{lastAppliedAnnotation: "{...}"}},
	})
	if _, ok := out["metadata"].(map[string]any)["annotations"]; ok {
		t.Error("the projection invented an empty annotations map the server does not have")
	}
}

// Redaction is decided by the KIND, never by what a value looks like. A ConfigMap holding a string
// that happens to read like a mask is an ordinary ConfigMap holding an ordinary string.
func TestRedactionIsDecidedByKindAndNeverByAValuesShape(t *testing.T) {
	out, paths := project(ProjectionFull, cm("u1", "1", map[string]any{"greeting": "**REDACTED**"}))
	if len(paths) != 0 {
		t.Errorf("a ConfigMap value was reported as redacted because of how it LOOKS: %v", paths)
	}
	if got := out["data"].(map[string]any)["greeting"]; got != "**REDACTED**" {
		t.Errorf("a real ConfigMap value was mangled: %v", got)
	}
}

// Keys-only disclosure: you learn THAT `token` exists — from redacted — and
// the value is not on the wire in any form. Not even as a mask.
//
// The mask is what this test used to assert, and it was the one poisoned value in the system: a
// browser holding `**REDACTED**` can save it back, and the literal string lands on the real secret.
// So the assertion is now the strong one — there is NOTHING there.
func TestSecretDisclosureIsKeysOnlyAndTheValueIsGone(t *testing.T) {
	secret := KRMObject{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata": map[string]any{"uid": "s1", "name": "git-creds", "labels": map[string]any{"a/b.c": "x"}},
		// A Secret's whole point is that these are secret. They are base64 of "hunter2" and "bob",
		// and the assertion below is that neither ever reaches the wire, in any form.
		"data": map[string]any{"token": "aHVudGVyMg==", "user~name": "Ym9i"}, //nolint:gosec // fixture data, and that is the test
	}
	out, paths := project(ProjectionFull, secret)

	// `data` is GONE, not left behind as an empty map. Every value in it was redacted, so the map
	// emptied, so the map goes with them — the same rule the annotations block applies, and for the
	// same reason: a map that is empty only because WE emptied it is our artifact, not the server's
	// state. `data: {}` would assert "this Secret has an empty data map", which is a different fact
	// from "this Secret's data is not yours to see".
	if _, present := out["data"]; present {
		t.Errorf("`data: {}` survived the projection: %v — an empty map WE created is our artifact, "+
			"not the server's state. (`ignore` on `status` would not leave `status: {}` either.)", out["data"])
	}

	// RFC 6901: `~` escapes to `~0`. A key with a tilde in it is legal, and an unescaped pointer
	// silently addresses the wrong field — the same class of bug as the client's dotted paths.
	want := []string{"/data/token", "/data/user~0name"}
	if len(paths) != 2 || paths[0].path != want[0] || paths[1].path != want[1] {
		t.Errorf("redacted: want %v, got %v", want, paths)
	}
	if _, ok := out["metadata"].(map[string]any)["labels"]; !ok {
		t.Error("labels must stay editable on a redacted Secret — that is the point of keys-only")
	}

	// And the upstream object is untouched: project() deep-copies, so the REAL secret is still in the
	// caller's object. Deleting from a shared informer's cache would be a spectacular bug.
	if secret["data"].(map[string]any)["token"] != "aHVudGVyMg==" {
		t.Error("project() mutated the upstream object — it deleted the real Secret's value")
	}

	// krm-raw/v1 declares no Secret policy. It must not silently apply one either — a projection's
	// removal rules are what it SAYS they are, or the identifier is worthless.
	rawOut, rawPaths := project(ProjectionRaw, secret)
	if rawOut["data"].(map[string]any)["token"] != "aHVudGVyMg==" || len(rawPaths) != 0 {
		t.Error("krm-raw/v1 redacted a value it does not declare redacting")
	}
}

// The other half of the rule, and the half that keeps it honest: we remove what WE removed, and
// nothing else. A Secret that genuinely arrived with an empty `data` map KEEPS it — that emptiness is
// the server's fact, not our artifact, and deleting it would be inventing a removal we did not make.
func TestAnEmptyDataMapTheServerSentIsKept(t *testing.T) {
	out, paths := project(ProjectionFull, KRMObject{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata": map[string]any{"uid": "s2", "name": "empty"},
		"data":     map[string]any{}, // the SERVER's empty map
	})

	if _, present := out["data"]; !present {
		t.Error("an empty `data` map the SERVER sent was deleted — that is not a redaction we made, " +
			"and removing it reports a removal that did not happen")
	}
	if len(paths) != 0 {
		t.Errorf("redacted = %v, want empty: there was nothing to redact", paths)
	}
}

func TestSpecProjectionSuppressesStatusOnlyUpdates(t *testing.T) {
	base := KRMObject{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"uid": "d1", "name": "web", "resourceVersion": "1"},
		"spec":     map[string]any{"replicas": 2},
		"status":   map[string]any{"readyReplicas": 1},
	}
	changed := deepCopyObject(base)
	changed["metadata"].(map[string]any)["resourceVersion"] = "2"
	changed["status"].(map[string]any)["readyReplicas"] = 2

	got := run(t, ProjectionSpec, AllowAll{}, []WatchEvent{
		{Type: WatchAdded, Object: base},
		{Type: WatchBookmark, InitialEventsEnd: true},
		{Type: WatchModified, Object: changed},
	}, 3)
	if !equalTypes(types(got), EventReset, EventAdded, EventSynced) {
		t.Fatalf("status-only update escaped krm-spec/v1 suppression: %v", types(got))
	}
	if _, ok := got[1].Object["status"]; ok {
		t.Fatal("krm-spec/v1 sent status")
	}
}

func TestRedactedValueChangeBumpsRevisionAndIsNotSuppressed(t *testing.T) {
	secret := func(rv, token string) KRMObject {
		return KRMObject{
			"apiVersion": "v1", "kind": "Secret",
			"metadata": map[string]any{"uid": "s1", "name": "credentials", "resourceVersion": rv},
			"data":     map[string]any{"token": token},
		}
	}
	got := run(t, ProjectionFull, AllowAll{}, []WatchEvent{
		{Type: WatchAdded, Object: secret("1", "old")},
		{Type: WatchBookmark, InitialEventsEnd: true},
		{Type: WatchModified, Object: secret("2", "new")},
	}, 4)
	if got[1].Redacted[0] != (Redaction{Path: "/data/token", Rev: 1}) ||
		got[3].Redacted[0] != (Redaction{Path: "/data/token", Rev: 2}) {
		t.Fatalf("redaction revisions = %#v then %#v, want 1 then 2", got[1].Redacted, got[3].Redacted)
	}
}

func TestSequenceIsGapFreeAndProjectionRequestIsAuthorized(t *testing.T) {
	got := run(t, ProjectionFull, AllowAll{}, []WatchEvent{
		{Type: WatchBookmark, InitialEventsEnd: true},
	}, 2)
	for i, event := range got {
		if event.Seq != uint64(i+1) {
			t.Fatalf("event %d sequence = %d, want %d", i, event.Seq, i+1)
		}
	}

	backend := &stubBackend{events: []WatchEvent{{Type: WatchBookmark, InitialEventsEnd: true}}}
	gw := &Gateway{Auth: AllowAll{}, Projection: ProjectionFull, Clients: func(string, Principal) (Backend, error) { return backend, nil }}
	sink := &recordingSink{}
	if err := gw.StreamProjection(t.Context(), nil, Scope{Target: "demo"}, ProjectionRaw, sink); err == nil {
		t.Fatal("an unauthorized raw projection request succeeded")
	}
	if backend.watches != 0 || len(sink.events) != 1 || sink.events[0].Code != CodeForbidden {
		t.Fatalf("raw request opened a watch or was not refused: watches=%d events=%#v", backend.watches, sink.events)
	}
}

// Denial comes before any watch opens. A gateway that opens the watch and filters afterwards has
// already leaked the object's existence to someone who may not see it.
func TestAuthorizerDeniesBeforeTheWatchIsEverOpened(t *testing.T) {
	backend := &stubBackend{events: []WatchEvent{{Type: WatchBookmark, InitialEventsEnd: true}}}
	gw := &Gateway{
		Auth: AuthorizerFunc(func(context.Context, Principal, Scope) error {
			return Forbidden("this scope is not yours")
		}),
		Clients: func(string, Principal) (Backend, error) { return backend, nil },
	}
	sink := &recordingSink{}
	err := gw.Stream(t.Context(), nil, Scope{Target: "demo"}, sink)

	if err == nil {
		t.Fatal("a denied stream must return the error it emitted")
	}
	if backend.watches != 0 {
		t.Error("the watch was opened for a caller who is not allowed to see the scope")
	}
	if len(sink.events) != 1 || sink.events[0].Type != EventError || sink.events[0].Code != CodeForbidden {
		t.Fatalf("want a single FORBIDDEN error; got %v", sink.events)
	}
	// Terminal, and it must SAY so: a browser's EventSource reconnects automatically otherwise, and
	// will hammer a scope it can never be allowed to see, forever.
	if !sink.events[0].Terminal {
		t.Error("FORBIDDEN must be terminal")
	}
	b, _ := json.Marshal(sink.events[0])
	if string(b) == "" {
		t.Fatal("unreachable")
	}
}
