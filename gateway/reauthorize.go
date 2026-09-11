package gateway

import (
	"context"
	"errors"
	"sync"
	"time"
)

// The gate serializes disclosure with reauthorization. While a check is pending, this subscriber
// receives no objects. Other subscribers and the shared upstream continue independently.
type authorizationSink struct {
	mu   sync.Mutex
	sink Sink
}

func (s *authorizationSink) Emit(ctx context.Context, event Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.sink.Emit(ctx, event)
}

func (g *Gateway) authorizedCycle(ctx context.Context, principal Principal, scope Scope, requested Projection, policy ProjectionPolicy, backend Backend, projection Projection, revisions map[string]map[string]redactionState, sink Sink) error {
	if g.ReauthorizationInterval <= 0 {
		return g.cycle(ctx, backend, scope, projection, revisions, sink)
	}
	cycleCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	gate := &authorizationSink{sink: sink}
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(g.ReauthorizationInterval)
		defer ticker.Stop()
		for {
			select {
			case <-cycleCtx.Done():
				done <- nil
				return
			case <-ticker.C:
				gate.mu.Lock()
				timeout := g.ReauthorizationTimeout
				if timeout <= 0 {
					timeout = 10 * time.Second
				}
				checkCtx, finish := context.WithTimeout(cycleCtx, timeout)
				err := g.Auth.Authorize(checkCtx, principal, scope)
				if err == nil {
					var selected Projection
					selected, err = policy.SelectProjection(checkCtx, principal, scope, requested)
					if err == nil && selected != projection {
						err = Forbidden("stream projection authorization changed; open a new stream")
					}
				}
				if err == nil {
					err = checkCtx.Err()
				}
				// Inspect cancellation before finish cancels the check itself. Teardown of a
				// recoverable cycle must not replace its original error with a terminal refusal.
				teardown := cycleCtx.Err() != nil && errors.Is(err, context.Canceled)
				finish()
				if teardown {
					gate.mu.Unlock()
					done <- nil
					return
				}
				if err != nil {
					// Fail closed even if a host returns a normally recoverable StreamError.
					se := *asStreamError(err)
					se.Terminal = true
					cancel()
					gate.mu.Unlock()
					done <- &se
					return
				}
				gate.mu.Unlock()
			}
		}
	}()
	err := g.cycle(cycleCtx, backend, scope, projection, revisions, gate)
	cancel()
	if authErr := <-done; authErr != nil {
		return authErr
	}
	return err
}
