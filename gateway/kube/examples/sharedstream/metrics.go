package sharedstream

import (
	"sync/atomic"

	"github.com/ConfigButler/krm-stream/gateway"
)

// Counters illustrates a low-cardinality Observer mapping. Export these atomics
// through the host's metrics endpoint. None measure physical API-server watches.
// The same observer can be used by both the gateway and its shared backend.
type Counters struct {
	Streams           atomic.Int64
	Subscriptions     atomic.Int64
	TransportRejected atomic.Int64
}

// Observe updates gauges synchronously; unknown kinds are intentionally ignored.
func (c *Counters) Observe(o gateway.Observation) {
	switch o.Kind {
	case gateway.ObservationStreamOpened:
		c.Streams.Add(1)
	case gateway.ObservationStreamClosed:
		c.Streams.Add(-1)
	case gateway.ObservationSharedSubscriptionOpened:
		c.Subscriptions.Add(1)
	case gateway.ObservationSharedSubscriptionClosed:
		c.Subscriptions.Add(-1)
	case gateway.ObservationHTTPTransportRejected:
		c.TransportRejected.Add(1)
	}
}
