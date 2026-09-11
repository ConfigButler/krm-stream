package gateway

// ObservationKind identifies a low-cardinality lifecycle signal. It intentionally never contains an
// object, patch, principal, or error message: hosts can turn these into metrics without exporting
// resource data or high-cardinality labels.
type ObservationKind string

const (
	// ObservationStreamOpened reports StreamProjection entry, before authorization.
	// HTTP identity/scope refusals before entry are excluded.
	ObservationStreamOpened ObservationKind = "stream_opened"
	// ObservationStreamClosed pairs each logical stream open with return after cleanup.
	ObservationStreamClosed ObservationKind = "stream_closed"
	// ObservationHTTPTransportRejected reports a failed bounded-HTTP capability check.
	// No logical stream opens and no SSE delivery is attempted.
	ObservationHTTPTransportRejected ObservationKind = "http_transport_rejected"
	// ObservationSharedSubscriptionOpened reports an attachment, including warm-cache joins.
	ObservationSharedSubscriptionOpened ObservationKind = "shared_subscription_opened"
	// ObservationSharedSubscriptionClosed reports an attachment becoming inactive once.
	// Overflow, scope death and leave end attachment; owned cleanup may finish later.
	ObservationSharedSubscriptionClosed ObservationKind = "shared_subscription_closed"
	// ObservationCycleStarted reports a new snapshot cycle after authorization and projection selection.
	ObservationCycleStarted ObservationKind = "cycle_started"
	// ObservationEventEmitted reports a protocol event accepted by the consumer sink.
	ObservationEventEmitted ObservationKind = "event_emitted"
	// ObservationEventSuppressed reports an upstream object update removed by the selected projection.
	ObservationEventSuppressed ObservationKind = "event_suppressed"
	// ObservationConsumerResync reports a live connection beginning a new snapshot cycle.
	ObservationConsumerResync ObservationKind = "consumer_resync"
	// ObservationSharedOverflow reports a shared-watch subscriber exceeding its bounded queue.
	ObservationSharedOverflow ObservationKind = "shared_overflow"
	// ObservationTerminalError reports a logical-stream failure before attempting its
	// terminal frame. It does not guarantee delivery.
	ObservationTerminalError ObservationKind = "terminal_error"
)

// Observation describes a lifecycle signal. Scope contains high-cardinality data
// and must not be exported blindly as metric labels.
type Observation struct {
	Kind       ObservationKind
	Scope      Scope
	Projection Projection
	EventType  EventType
	Code       ErrorCode
}

// Observer receives stream lifecycle signals. Observe must return promptly; it runs on the stream or
// shared-watch goroutine, sometimes under shared locks. Calls are synchronous and
// may be concurrent; they are not dropped by the library. Observers must be thread-safe,
// must not panic or reenter the gateway, and should update counters synchronously.
// Each lifetime opens before closing, with no global ordering across lifetimes.
// A lossy exporter queue must not be the source of truth for lifetime gauges.
type Observer interface {
	Observe(Observation)
}

// ObserverFunc adapts a function for small hosts and tests.
type ObserverFunc func(Observation)

// Observe calls f.
func (f ObserverFunc) Observe(observation Observation) {
	f(observation)
}
