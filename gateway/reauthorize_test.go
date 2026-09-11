package gateway

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type reauthEvents chan Event

func (s reauthEvents) Emit(ctx context.Context, ev Event) error {
	select {
	case s <- ev:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func awaitEvent(t *testing.T, events reauthEvents, kind EventType) Event {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case ev := <-events:
			if ev.Type == kind {
				return ev
			}
		case <-timer.C:
			t.Fatalf("waiting for %s", kind)
		}
	}
}

func TestTimedRevocationOnlyDisconnectsDeniedSharedSubscriber(t *testing.T) {
	upstream := newFakeUpstream()
	shared := NewSharedBackend(upstream)
	var revoked atomic.Bool
	g := &Gateway{
		Auth: AuthorizerFunc(func(_ context.Context, p Principal, _ Scope) error {
			if p == "alice" && revoked.Load() {
				return Forbidden("revoked")
			}
			return nil
		}),
		Clients:                 func(context.Context, string, Principal) (Backend, error) { return shared, nil },
		ReauthorizationInterval: 5 * time.Millisecond,
	}
	alice, bob := make(reauthEvents, 32), make(reauthEvents, 32)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	aDone, bDone := make(chan error, 1), make(chan error, 1)
	go func() { aDone <- g.Stream(ctx, "alice", sharedScopeUnderTest, alice) }()
	go func() { bDone <- g.Stream(ctx, "bob", sharedScopeUnderTest, bob) }()
	upstream.send(WatchEvent{Type: WatchBookmark, InitialEventsEnd: true})
	awaitEvent(t, alice, EventSynced)
	awaitEvent(t, bob, EventSynced)
	revoked.Store(true)
	denied := awaitEvent(t, alice, EventError)
	if denied.Code != CodeForbidden || !denied.Terminal {
		t.Fatalf("not terminal denial: %+v", denied)
	}
	<-aDone
	upstream.send(WatchEvent{Type: WatchAdded, Object: obj("u", "cm", "1")})
	awaitEvent(t, bob, EventAdded)
	if upstream.opens() != 1 {
		t.Fatal("revocation reopened shared upstream")
	}
	cancel()
	<-bDone
}

func TestTimedAuthorizationTimeoutFailsClosed(t *testing.T) {
	var calls atomic.Int32
	upstream := newFakeUpstream()
	g := &Gateway{
		Auth: AuthorizerFunc(func(ctx context.Context, _ Principal, _ Scope) error {
			if calls.Add(1) == 1 {
				return nil
			}
			<-ctx.Done()
			return ctx.Err()
		}),
		Clients:                 func(context.Context, string, Principal) (Backend, error) { return upstream, nil },
		ReauthorizationInterval: time.Millisecond, ReauthorizationTimeout: 5 * time.Millisecond,
	}
	events := make(reauthEvents, 32)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- g.Stream(ctx, "alice", sharedScopeUnderTest, events) }()
	ev := awaitEvent(t, events, EventError)
	if !ev.Terminal || ev.Code != CodeInternal {
		t.Fatalf("timeout did not fail closed: %+v", ev)
	}
	<-done
}

func TestTimedProjectionWithdrawalTerminatesStream(t *testing.T) {
	var checks atomic.Int32
	g := &Gateway{
		Auth: AllowAll{},
		Projections: ProjectionPolicyFunc(func(context.Context, Principal, Scope, Projection) (Projection, error) {
			if checks.Add(1) == 1 {
				return ProjectionRaw, nil
			}
			return ProjectionFull, nil
		}),
		Clients:                 func(context.Context, string, Principal) (Backend, error) { return newFakeUpstream(), nil },
		ReauthorizationInterval: time.Millisecond,
	}
	events := make(reauthEvents, 32)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- g.Stream(ctx, "alice", sharedScopeUnderTest, events) }()
	ev := awaitEvent(t, events, EventError)
	if ev.Code != CodeForbidden || !ev.Terminal {
		t.Fatalf("projection change kept old view: %+v", ev)
	}
	<-done
}

func TestTimedCheckPausesDisclosureAndCancellationStopsCheck(t *testing.T) {
	upstream := newFakeUpstream()
	var calls atomic.Int32
	checking := make(chan struct{})
	canceled := make(chan struct{})
	g := &Gateway{
		Auth: AuthorizerFunc(func(ctx context.Context, _ Principal, _ Scope) error {
			if calls.Add(1) == 1 {
				return nil
			}
			close(checking)
			<-ctx.Done()
			close(canceled)
			return ctx.Err()
		}),
		Clients:                 func(context.Context, string, Principal) (Backend, error) { return upstream, nil },
		ReauthorizationInterval: time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	events := make(reauthEvents, 32)
	done := make(chan error, 1)
	go func() { done <- g.Stream(ctx, "alice", sharedScopeUnderTest, events) }()
	awaitEvent(t, events, EventReset)
	select {
	case <-checking:
	case <-ctx.Done():
		t.Fatal("check never started")
	}
	upstream.send(WatchEvent{Type: WatchAdded, Object: obj("u", "cm", "1")})
	select {
	case ev := <-events:
		t.Fatalf("disclosed while authorization pending: %+v", ev)
	case <-time.After(10 * time.Millisecond):
	}
	cancel()
	<-done
	select {
	case <-canceled:
	default:
		t.Fatal("check outlived stream")
	}
}

// A cycle ending while its recheck is blocked must retain its original recovery outcome.
func TestCycleTeardownDoesNotBecomeAuthorizationFailure(t *testing.T) {
	for _, cycleErr := range []error{ErrWatchClosed, ResyncRequired("queue overflow"), &StreamError{Code: CodeSlowConsumer, Terminal: false, Message: "slow"}} {
		t.Run(cycleErr.Error(), func(t *testing.T) {
			checking := make(chan struct{})
			backend := &reauthClosingBackend{checking: checking, err: cycleErr}
			g := &Gateway{ReauthorizationInterval: time.Millisecond, Auth: AuthorizerFunc(func(ctx context.Context, _ Principal, _ Scope) error {
				close(checking)
				<-ctx.Done()
				return ctx.Err()
			})}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			err := g.authorizedCycle(ctx, "alice", sharedScopeUnderTest, "", StaticProjection(ProjectionFull), backend, ProjectionFull, map[string]map[string]redactionState{}, make(reauthEvents, 8))
			if err != cycleErr {
				t.Fatalf("cycle outcome replaced: got %v, want %v", err, cycleErr)
			}
		})
	}
}

type reauthClosingBackend struct {
	checking <-chan struct{}
	err      error
}

func (b *reauthClosingBackend) Watch(context.Context, Scope) (Watcher, error) { return b, nil }
func (b *reauthClosingBackend) Next(ctx context.Context) (WatchEvent, error) {
	select {
	case <-b.checking:
		return WatchEvent{}, b.err
	case <-ctx.Done():
		return WatchEvent{}, ctx.Err()
	}
}
func (*reauthClosingBackend) Stop() {}

func TestExplicitDenialSurvivesConcurrentCycleTeardown(t *testing.T) {
	checking := make(chan struct{})
	backend := &reauthClosingBackend{checking: checking, err: ErrWatchClosed}
	denial := Forbidden("revoked")
	g := &Gateway{ReauthorizationInterval: time.Millisecond, Auth: AuthorizerFunc(func(ctx context.Context, _ Principal, _ Scope) error {
		close(checking)
		<-ctx.Done()
		return denial
	})}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := g.authorizedCycle(ctx, "alice", sharedScopeUnderTest, "", StaticProjection(ProjectionFull), backend, ProjectionFull, map[string]map[string]redactionState{}, make(reauthEvents, 8))
	se, ok := err.(*StreamError)
	if !ok || !se.Terminal || se.Code != CodeForbidden {
		t.Fatalf("lost explicit denial: %v", err)
	}
}

func TestStreamRecoversAfterTeardownDuringReauthorization(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	checking := make(chan struct{})
	var checks, cycles atomic.Int32
	fresh := newFakeUpstream()
	g := &Gateway{
		Auth: AuthorizerFunc(func(ctx context.Context, _ Principal, _ Scope) error {
			if checks.Add(1) == 2 {
				close(checking)
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		}),
		Clients: func(context.Context, string, Principal) (Backend, error) {
			if cycles.Add(1) == 1 {
				return &reauthClosingBackend{checking: checking, err: ErrWatchClosed}, nil
			}
			return fresh, nil
		},
		ReauthorizationInterval: time.Millisecond,
	}
	events := make(reauthEvents, 32)
	done := make(chan error, 1)
	go func() { done <- g.Stream(ctx, "alice", sharedScopeUnderTest, events) }()
	// The first watcher cannot close until its timed check is in flight.
	for _, kind := range []EventType{EventReset, EventError, EventReset} {
		select {
		case ev := <-events:
			if ev.Type != kind || ev.Terminal {
				t.Fatalf("want nonterminal %s, got %+v", kind, ev)
			}
			if kind == EventError && ev.Code != CodeResyncRequired {
				t.Fatalf("want resync, got %+v", ev)
			}
		case <-ctx.Done():
			t.Fatal("stream did not recover")
		}
	}
	fresh.send(WatchEvent{Type: WatchBookmark, InitialEventsEnd: true})
	select {
	case ev := <-events:
		if ev.Type != EventSynced {
			t.Fatalf("want synced after recovery, got %+v", ev)
		}
	case <-ctx.Done():
		t.Fatal("fresh cycle did not sync")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream outlived cancellation")
	}
}
