package gateway

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
)

// One name per setting. Options and Gateway embed StreamConfig instead of each declaring the shared
// settings, so Handler copies one value and the two entry points cannot drift apart again.
func TestOptionsAndGatewayShareOneStreamConfig(t *testing.T) {
	exported := func(v any) []string {
		var out []string
		typ := reflect.TypeOf(v)
		for i := range typ.NumField() {
			if f := typ.Field(i); f.IsExported() {
				out = append(out, f.Name)
			}
		}
		return out
	}
	options, gw := exported(Options{}), exported(Gateway{})
	if !slices.Equal(gw, []string{"StreamConfig"}) {
		t.Errorf("Gateway exports %v; its settings belong in StreamConfig", gw)
	}
	if !slices.Equal(options, []string{"StreamConfig", "Principal", "Scopes"}) {
		t.Errorf("Options exports %v; only the HTTP-only settings belong beside StreamConfig", options)
	}
}

// stopAfterSynced records events and ends the stream at the nth synced: a sink error unwinds the
// loop the way a closed connection does.
type stopAfterSynced struct {
	n      int
	events []Event
}

var errSyncedEnough = errors.New("seen enough snapshots")

func (s *stopAfterSynced) Emit(_ context.Context, ev Event) error {
	if s.n == 0 {
		return errSyncedEnough // closed: the gateway's report of it is not delivered either
	}
	s.events = append(s.events, ev)
	if ev.Type == EventSynced {
		if s.n--; s.n == 0 {
			return errSyncedEnough
		}
	}
	return nil
}

// The zero StreamConfig is the safe one: with no Projections policy the stream is krm-full/v1, which
// withholds Secret values, and a request for any other view is refused before a watch opens.
func TestNilProjectionPolicyGrantsOnlyTheRedactingDefault(t *testing.T) {
	secret := KRMObject{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"uid": "s1", "name": "creds", "resourceVersion": "1"},
		"data":     map[string]any{"token": "aHVudGVyMg=="}, //nolint:gosec // fixture data, and that is the test
	}
	open := func(requested Projection) (*stopAfterSynced, *stubBackend, error) {
		backend := &stubBackend{events: []WatchEvent{{Type: WatchAdded, Object: secret}, {Type: WatchBookmark, InitialEventsEnd: true}}}
		g := &Gateway{StreamConfig: StreamConfig{
			Authorizer: AllowAll{},
			Clients:    func(context.Context, string, Principal) (Backend, error) { return backend, nil },
		}}
		sink := &stopAfterSynced{n: 1}
		err := g.Stream(t.Context(), nil, Scope{Target: "demo", Version: "v1", Resource: "secrets"}, requested, sink)
		return sink, backend, err
	}

	for _, requested := range []Projection{"", ProjectionFull} {
		sink, _, err := open(requested)
		if !errors.Is(err, errSyncedEnough) || !equalTypes(types(sink.events), EventReset, EventAdded, EventSynced) {
			t.Fatalf("requested %q: err=%v events=%v", requested, err, types(sink.events))
		}
		if sink.events[0].Projection != ProjectionFull {
			t.Errorf("requested %q: reset announced %q, want %q", requested, sink.events[0].Projection, ProjectionFull)
		}
		added := sink.events[1]
		if _, present := added.Object["data"]; present || len(added.Redacted) != 1 || added.Redacted[0].Path != "/data/token" {
			t.Errorf("requested %q: the Secret value was not withheld: object=%v redacted=%v", requested, added.Object, added.Redacted)
		}
	}

	sink, backend, err := open(ProjectionRaw)
	if err == nil || len(sink.events) != 1 || sink.events[0].Code != CodeForbidden || !sink.events[0].Terminal {
		t.Fatalf("a raw request under the default policy: err=%v events=%+v", err, sink.events)
	}
	if backend.watches != 0 {
		t.Error("the watch opened for a projection the policy refused")
	}
}

// A dynamic policy decides at opening and again on every snapshot cycle, with the caller's request
// each time, and each reset announces what it chose. (Timed checks: TestTimedProjectionWithdrawal…)
func TestProjectionPolicyDecidesAtOpeningAndEveryCycle(t *testing.T) {
	var asked []Projection
	policy := ProjectionPolicyFunc(func(_ context.Context, _ Principal, _ Scope, requested Projection) (Projection, error) {
		asked = append(asked, requested)
		if len(asked) == 1 {
			return ProjectionRaw, nil
		}
		return ProjectionSpec, nil
	})
	opens := 0
	g := &Gateway{StreamConfig: StreamConfig{
		Authorizer:  AllowAll{},
		Projections: policy,
		Clients: func(context.Context, string, Principal) (Backend, error) {
			return backendFunc(func() (Watcher, error) {
				opens++
				if opens == 1 {
					return &stubWatcher{events: []WatchEvent{
						{Type: WatchBookmark, InitialEventsEnd: true},
						{Type: WatchError, Err: ResyncRequired("expired")},
					}}, nil
				}
				return &stubWatcher{events: []WatchEvent{{Type: WatchBookmark, InitialEventsEnd: true}}}, nil
			}), nil
		},
	}}
	sink := &stopAfterSynced{n: 2}
	if err := g.Stream(t.Context(), "alice", Scope{Target: "demo", Version: "v1", Resource: "configmaps"}, ProjectionRaw, sink); !errors.Is(err, errSyncedEnough) {
		t.Fatalf("stream: %v (events %v)", err, types(sink.events))
	}
	if !slices.Equal(asked, []Projection{ProjectionRaw, ProjectionRaw}) {
		t.Errorf("the policy was asked %v; want the caller's request at opening and at the second cycle", asked)
	}
	var announced []Projection
	for _, ev := range sink.events {
		if ev.Type == EventReset {
			announced = append(announced, ev.Projection)
		}
	}
	if !slices.Equal(announced, []Projection{ProjectionRaw, ProjectionSpec}) {
		t.Errorf("resets announced %v; want each cycle's own decision", announced)
	}
}
