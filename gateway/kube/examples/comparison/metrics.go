package comparison

import (
	"bytes"
	"maps"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ConfigButler/krm-stream/gateway"
)

// Metrics is the host's own record of what each source cost. Every key is built from constants, a
// route name, an event type or an HTTP status: never a UID, a user, a namespace or a resourceVersion,
// so the set of keys is small and fixed by the code, as a real metrics backend needs it to be.
//
// Counters reset with Reset; gauges (what is open right now) do not, because the connections they
// count outlive any measurement window.
type Metrics struct {
	mu       sync.Mutex
	since    time.Time
	counters map[string]int64
	gauges   map[string]int64
}

// NewMetrics starts an empty record.
func NewMetrics() *Metrics {
	return &Metrics{since: time.Now(), counters: map[string]int64{}, gauges: map[string]int64{}}
}

// Add increments a counter.
func (m *Metrics) Add(key string, n int64) {
	m.mu.Lock()
	m.counters[key] += n
	m.mu.Unlock()
}

// Gauge moves a gauge by delta.
func (m *Metrics) Gauge(key string, delta int64) {
	m.mu.Lock()
	m.gauges[key] += delta
	m.mu.Unlock()
}

// Snapshot is the JSON the metrics endpoint serves.
type Snapshot struct {
	Since     time.Time        `json:"since"`
	ElapsedMs int64            `json:"elapsedMs"`
	Counters  map[string]int64 `json:"counters"`
	Gauges    map[string]int64 `json:"gauges"`
}

// Snapshot copies the current values.
func (m *Metrics) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Snapshot{
		Since:     m.since,
		ElapsedMs: time.Since(m.since).Milliseconds(),
		Counters:  maps.Clone(m.counters),
		Gauges:    maps.Clone(m.gauges),
	}
}

// Reset clears the counters and starts a new window. Gauges are kept.
func (m *Metrics) Reset() {
	m.mu.Lock()
	m.counters = map[string]int64{}
	m.since = time.Now()
	m.mu.Unlock()
}

// observer maps gateway Observations onto one route's counters. Observations carry a Scope, which
// holds a namespace and a name; only the kind and the event type become part of a key.
func observer(m *Metrics, route string) gateway.Observer {
	return gateway.ObserverFunc(func(o gateway.Observation) {
		prefix := "gateway." + route + ".obs."
		switch o.Kind {
		case gateway.ObservationEventEmitted, gateway.ObservationEventSuppressed:
			m.Add(prefix+string(o.Kind)+"."+string(o.EventType), 1)
		case gateway.ObservationStreamOpened:
			m.Add(prefix+string(o.Kind), 1)
			m.Gauge("gateway."+route+".streams_active", 1)
		case gateway.ObservationStreamClosed:
			m.Add(prefix+string(o.Kind), 1)
			m.Gauge("gateway."+route+".streams_active", -1)
		case gateway.ObservationSharedSubscriptionOpened:
			m.Add(prefix+string(o.Kind), 1)
			m.Gauge("gateway."+route+".shared_subscriptions_active", 1)
		case gateway.ObservationSharedSubscriptionClosed:
			m.Add(prefix+string(o.Kind), 1)
			m.Gauge("gateway."+route+".shared_subscriptions_active", -1)
		case gateway.ObservationRetryableError, gateway.ObservationTerminalError:
			m.Add(prefix+string(o.Kind)+"."+string(o.Code), 1)
		default:
			m.Add(prefix+string(o.Kind), 1)
		}
	})
}

// countingWriter counts the bytes a handler sends downstream. It keeps the capabilities the gateway
// and the reverse proxy rely on: FlushError for every frame, and Unwrap so http.ResponseController
// reaches the server's write deadline.
type countingWriter struct {
	http.ResponseWriter
	add    func(int64)
	status int
	// frames, when set, receives each written chunk so a native watch's frame types can be counted.
	frames func([]byte)
}

func (w *countingWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *countingWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.add(int64(n))
	if w.frames != nil && n > 0 {
		w.frames(p[:n])
	}
	return n, err
}

func (w *countingWriter) FlushError() error {
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *countingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// watchFrameTypes are the native watch event types counted on the way to the browser. The API server
// writes one JSON object per line, each starting with its type.
var watchFrameTypes = []string{"ADDED", "MODIFIED", "DELETED", "BOOKMARK", "ERROR"}

// countFrames counts native watch frames by type in one written chunk. A frame split across two
// writes would be missed; the API server writes and flushes each event whole.
func countFrames(m *Metrics, chunk []byte) {
	for _, kind := range watchFrameTypes {
		if n := bytes.Count(chunk, []byte(`{"type":"`+kind+`"`)); n > 0 {
			m.Add("native.watch_frames."+kind, int64(n))
		}
	}
}

// statusClass keeps HTTP status labels bounded: the exact code for the few that matter here, a
// class for the rest.
func statusClass(code int) string {
	switch code {
	case 200, 201, 204, 400, 401, 403, 404, 409, 410, 415, 422, 502:
		return strconv.Itoa(code)
	}
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	default:
		return "other"
	}
}
