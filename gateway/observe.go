package gateway

// ObservationKind identifies a low-cardinality lifecycle signal. It intentionally never contains an
// object, patch, principal, or error message: hosts can turn these into metrics without exporting
// resource data or high-cardinality labels.
type ObservationKind string

const (
	// ObservationStreamOpened reports a newly accepted stream request.
	ObservationStreamOpened ObservationKind = "stream_opened"
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
	// ObservationTerminalError reports a stream-ending error code.
	ObservationTerminalError ObservationKind = "terminal_error"
)

// Observation is one safe-to-export stream lifecycle signal.
type Observation struct {
	Kind       ObservationKind
	Scope      Scope
	Projection Projection
	EventType  EventType
	Code       ErrorCode
}

// Observer receives stream lifecycle signals. Observe must return promptly; it runs on the stream or
// shared-watch goroutine. Use it to increment counters or enqueue into a non-blocking exporter.
type Observer interface {
	Observe(Observation)
}

// ObserverFunc adapts a function for small hosts and tests.
type ObserverFunc func(Observation)

// Observe calls f.
func (f ObserverFunc) Observe(observation Observation) {
	f(observation)
}
