package comparison

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"time"
)

// A workload is a deterministic schedule of writes and forced downstream disconnects. The same
// request always yields the same schedule, so every source — and every repetition, and the baseline
// and current library — sees identical objects and identical churn.

// The workload names.
const (
	WorkloadQuiet            = "quiet"
	WorkloadBurst            = "burst"
	WorkloadChurn            = "churn"
	WorkloadForcedDisconnect = "forced-disconnect"
)

// Workloads lists the names Plan accepts.
var Workloads = []string{WorkloadQuiet, WorkloadBurst, WorkloadChurn, WorkloadForcedDisconnect}

// WorkloadRequest parameterizes a workload. Zero values take the defaults documented on each field.
type WorkloadRequest struct {
	Name string `json:"name"`
	// DurationMs is how long the schedule runs. Default 20000.
	DurationMs int `json:"durationMs"`
	// Seed picks which objects each write touches. Default 1.
	Seed uint64 `json:"seed"`
	// Rate is the write rate per second: status writes for churn, mixed writes for
	// forced-disconnect, writes per burst for burst. Defaults 10, 4 and 12.
	Rate float64 `json:"rate"`
	// Reconnects is how many forced downstream disconnects every workload includes, spread evenly.
	// Default 2. forced-disconnect adds one every DisconnectEveryMs on top.
	Reconnects int `json:"reconnects"`
	// DisconnectEveryMs is forced-disconnect's period. Default 4000.
	DisconnectEveryMs int `json:"disconnectEveryMs"`
	// Target is which downstream connections a disconnect closes: native, gateway or all. Default all.
	Target string `json:"target"`
}

// StepKind is what one step does.
type StepKind string

// The step kinds.
const (
	StepSpec       StepKind = "spec"       // a spec write to a Widget
	StepStatus     StepKind = "status"     // a status-subresource write to a Widget
	StepSecret     StepKind = "secret"     // a Secret value rotation
	StepDisconnect StepKind = "disconnect" // close downstream connections
)

// Step is one scheduled action.
type Step struct {
	At     time.Duration `json:"atMs"`
	Kind   StepKind      `json:"kind"`
	Object int           `json:"object"`
	Seq    int           `json:"seq"`
}

// Plan is a request's complete schedule, sorted by time.
type Plan struct {
	Request WorkloadRequest `json:"request"`
	Steps   []Step          `json:"steps"`
}

// Counts tallies a plan's steps by kind.
func (p Plan) Counts() map[StepKind]int {
	counts := map[StepKind]int{}
	for _, s := range p.Steps {
		counts[s.Kind]++
	}
	return counts
}

func (r WorkloadRequest) withDefaults() WorkloadRequest {
	if r.DurationMs <= 0 {
		r.DurationMs = 20000
	}
	if r.Seed == 0 {
		r.Seed = 1
	}
	if r.Reconnects <= 0 {
		r.Reconnects = 2
	}
	if r.DisconnectEveryMs <= 0 {
		r.DisconnectEveryMs = 4000
	}
	if r.Target == "" {
		r.Target = "all"
	}
	if r.Rate <= 0 {
		switch r.Name {
		case WorkloadChurn:
			r.Rate = 10
		case WorkloadForcedDisconnect:
			r.Rate = 4
		case WorkloadBurst:
			r.Rate = 12
		}
	}
	return r
}

// NewPlan builds the schedule for a request against widgetCount Widgets and secretCount Secrets.
func NewPlan(req WorkloadRequest, widgetCount, secretCount int) (Plan, error) {
	req = req.withDefaults()
	if widgetCount < 1 || secretCount < 1 {
		return Plan{}, fmt.Errorf("comparison: a workload needs at least one Widget and one Secret")
	}
	switch req.Target {
	case "all", "native", "gateway":
	default:
		return Plan{}, fmt.Errorf("comparison: unknown disconnect target %q", req.Target)
	}
	if req.DurationMs > 10*60*1000 || req.Rate > 200 {
		return Plan{}, fmt.Errorf("comparison: duration or rate out of range")
	}
	duration := time.Duration(req.DurationMs) * time.Millisecond
	// Seeded on purpose: the point is the same schedule every time, not unpredictability.
	rng := rand.New(rand.NewPCG(req.Seed, uint64(len(req.Name)))) //nolint:gosec // see above
	var steps []Step
	add := func(at time.Duration, kind StepKind) {
		object := 0
		switch kind {
		case StepSpec, StepStatus:
			object = rng.IntN(widgetCount)
		case StepSecret:
			object = rng.IntN(secretCount)
		}
		steps = append(steps, Step{At: at, Kind: kind, Object: object})
	}
	every := func(period time.Duration, kind StepKind) {
		for at := period; at < duration; at += period {
			add(at, kind)
		}
	}
	perSecond := func(rate float64) time.Duration {
		return time.Duration(math.Round(float64(time.Second) / rate))
	}

	switch req.Name {
	case WorkloadQuiet:
		// No writes at all: what an idle page costs each source.
	case WorkloadBurst:
		// A burst of spec and status writes every 4 seconds, 20 ms apart, then silence.
		for start := 2 * time.Second; start < duration; start += 4 * time.Second {
			for i := range int(req.Rate) {
				kind := StepSpec
				if rng.IntN(2) == 1 {
					kind = StepStatus
				}
				add(start+time.Duration(i)*20*time.Millisecond, kind)
			}
		}
	case WorkloadChurn:
		// Sustained status-only writes, as a controller makes them, with an occasional spec write and
		// a Secret rotation. Status churn is what krm-spec/v1 suppresses; a rotation is what full and
		// spec show only as a redaction revision.
		every(perSecond(req.Rate), StepStatus)
		every(2*time.Second, StepSpec)
		every(3*time.Second, StepSecret)
	case WorkloadForcedDisconnect:
		// Moderate mixed writes, with downstream connections closed every few seconds.
		period := perSecond(req.Rate)
		kinds := []StepKind{StepSpec, StepStatus, StepStatus, StepSecret}
		for at := period / 2; at < duration; at += period {
			add(at, kinds[rng.IntN(len(kinds))])
		}
		every(time.Duration(req.DisconnectEveryMs)*time.Millisecond, StepDisconnect)
	default:
		return Plan{}, fmt.Errorf("comparison: unknown workload %q (want one of %v)", req.Name, Workloads)
	}
	// Every workload includes forced reconnects, so reconnect cost is measured under each one. They
	// are offset by a quarter second from the even split so they never coincide with a burst start.
	for i := 1; i <= req.Reconnects; i++ {
		steps = append(steps, Step{At: duration*time.Duration(i)/time.Duration(req.Reconnects+1) + 250*time.Millisecond, Kind: StepDisconnect})
	}
	slices.SortStableFunc(steps, func(a, b Step) int { return int(a.At - b.At) })
	for i := range steps {
		steps[i].Seq = i + 1
	}
	return Plan{Request: req, Steps: steps}, nil
}

// WorkloadResult reports what a run actually did.
type WorkloadResult struct {
	Plan        map[StepKind]int `json:"planned"`
	Done        map[StepKind]int `json:"done"`
	Closed      int              `json:"connectionsClosed"`
	Errors      []string         `json:"errors,omitempty"`
	MaxLateMs   int64            `json:"maxLateMs"`
	ElapsedMs   int64            `json:"elapsedMs"`
	DurationMs  int              `json:"durationMs"`
	Seed        uint64           `json:"seed"`
	Rate        float64          `json:"rate"`
	Reconnects  int              `json:"reconnects"`
	StartedUnix int64            `json:"startedUnixMs"`
}

// run executes a plan in time order on one goroutine. A step that falls behind runs at once; how far
// behind the worst one was is reported, so an overloaded run is visible rather than silently slower.
func (o *objects) run(ctx context.Context, plan Plan, disconnect func(target string) int) WorkloadResult {
	start := time.Now()
	result := WorkloadResult{
		Plan: plan.Counts(), Done: map[StepKind]int{},
		DurationMs: plan.Request.DurationMs, Seed: plan.Request.Seed, Rate: plan.Request.Rate,
		Reconnects: plan.Request.Reconnects, StartedUnix: start.UnixMilli(),
	}
	for _, step := range plan.Steps {
		if wait := time.Until(start.Add(step.At)); wait > 0 {
			select {
			case <-ctx.Done():
				result.Errors = append(result.Errors, ctx.Err().Error())
				return result
			case <-time.After(wait):
			}
		} else {
			result.MaxLateMs = max(result.MaxLateMs, (-wait).Milliseconds())
		}
		var err error
		switch step.Kind {
		case StepSpec:
			// Always a change: the revision is the step's sequence number.
			err = o.patch(ctx, o.widgetClient(), WidgetName(step.Object), map[string]any{"spec": map[string]any{
				"revision": int64(step.Seq), "replicas": int64(step.Seq%5 + 1),
			}})
		case StepStatus:
			err = o.patch(ctx, o.widgetClient(), WidgetName(step.Object), map[string]any{"status": map[string]any{
				"heartbeat": int64(step.Seq), "phase": []string{"Ready", "Progressing"}[step.Seq%2],
			}}, "status")
		case StepSecret:
			err = o.patch(ctx, o.secretClient(), SecretName(step.Object), map[string]any{"data": map[string]any{
				"token": base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "rotated-%d-%d", plan.Request.Seed, step.Seq)),
			}})
		case StepDisconnect:
			result.Closed += disconnect(plan.Request.Target)
		}
		if err != nil {
			result.Errors = append(result.Errors, err.Error())
			continue
		}
		result.Done[step.Kind]++
	}
	// The schedule's tail is quiet until its full duration, so every run lasts as long as it says.
	if wait := time.Until(start.Add(time.Duration(plan.Request.DurationMs) * time.Millisecond)); wait > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
	}
	result.ElapsedMs = time.Since(start).Milliseconds()
	return result
}
