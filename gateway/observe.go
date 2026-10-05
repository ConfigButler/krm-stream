package gateway

// ObservationKind identifies a low-cardinality lifecycle signal. It intentionally never contains an
// object, patch, principal, or error message: hosts can turn these into metrics without exporting
// resource data or high-cardinality labels.
type ObservationKind string

const (
	// ObservationStreamOpened reports Stream entry, before authorization.
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
	// ObservationRetryableError reports a logical stream ending with a non-terminal error the client
	// must reconnect for, such as UPSTREAM_UNAVAILABLE, before attempting its frame.
	ObservationRetryableError ObservationKind = "retryable_error"
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

// Diagnostic is the raw error behind an error event, for the host's logs. Unlike an Observation it
// carries the error itself, which may name internal addresses, URLs or anything else an upstream put
// in it. That is why it has a hook of its own: the host decides what to log and what to redact.
type Diagnostic struct {
	// Principal is nil when the caller could not be identified.
	Principal Principal
	// Scope is the zero Scope when the request was refused before a scope was parsed.
	Scope Scope
	// Code and Terminal are what the consumer was sent.
	Code     ErrorCode
	Terminal bool
	// Err is the complete error, including any StreamError.Cause.
	Err error
}

// Diagnostics receives a Diagnostic for every error event the gateway sends, and for every upstream
// error it recovers from with a new snapshot cycle. It runs on the stream goroutine and must return
// promptly.
type Diagnostics func(Diagnostic)
