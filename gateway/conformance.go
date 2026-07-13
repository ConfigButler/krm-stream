package gateway

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// The conformance loader. The Go and TypeScript suites read the SAME generated JSON — that shared
// read is the entire reason this is one repository. Keep the two loaders' semantics identical; if
// they drift, the contract is no longer a contract.
//
// Source of truth is ../conformance/{bodies,fixtures}/*.yaml. `task fixtures` builds the JSON; CI
// fails if it is stale.

// Fixture is one scenario, end to end: what the watch does, what must therefore appear on the wire,
// and what a client that consumed that wire must then hold.
type Fixture struct {
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	Why    string   `json:"why"`
	Suites []string `json:"suites"`

	Scope      *Scope     `json:"scope"`
	Projection Projection `json:"projection"`

	// Watch is the gateway's input: a scripted fake Kubernetes watch. No cluster required.
	Watch []WatchOp `json:"watch"`
	// Events is the shared surface — the exact wire. The gateway must emit it; the client is fed it.
	Events []FixtureEvent `json:"events"`
	// Client is the client suite's business; the gateway ignores it.
	Client json.RawMessage `json:"client"`

	// There was a GatewayRejects field here. It is gone, with the fixture block it parsed: krm-stream
	// has NO WRITE PATH (spec §3), so "saves the gateway must refuse" described a thing the gateway
	// cannot do. It was parsed and never read — a struct tag standing in for an implementation.
}

// WatchOp is one step of the scripted upstream.
//
//	list       the initial LIST a lister/informer sees (opens a snapshot cycle)
//	added      an object entered scope
//	modified   an object changed
//	deleted    an object left scope
//	relist     upstream continuity was LOST (410 Gone / cache reset) — a new cycle must follow,
//	           even though the SSE connection is perfectly healthy
//	disconnect the consumer's connection dropped; the next list is a fresh cycle
//
// And three that say what Kubernetes really does, added by docs/proposals/0001-watch-ops.md because
// the corpus could not otherwise express three of the gateway's own MUST NOTs:
//
//	bookmark   a routine BOOKMARK. Its object carries ONLY metadata.resourceVersion — that is not an
//	           edge case, it is what the API server sends, on every stream that asked for bookmarks.
//	           Absorbed: never forwarded, never mistaken for the snapshot boundary.
//	partial    a metadata-only object (PartialObjectMetadata) delivered as an upsert. Has a uid, has
//	           NO spec and NO status. Must be refused — forwarding it blanks the consumer's object.
//	tombstone  a DELETED whose object lost its identity (client-go's DeletedFinalStateUnknown).
//	           Must not be guessed at: a new snapshot cycle prunes correctly instead.
type WatchOp struct {
	Op     string   `json:"op"`
	Body   string   `json:"body"`   // a bodies/ reference, e.g. "cm-app.v2"
	Bodies []string `json:"bodies"` // for list/relist
	// ResourceVersion is the bookmark's payload — the only field a real BOOKMARK's object has.
	ResourceVersion string `json:"resourceVersion"`
}

// FixtureEvent is an Event with its object given by REFERENCE into bodies/, so a scenario stays
// readable. Resolve() turns it into the real thing.
type FixtureEvent struct {
	Type          EventType `json:"type"`
	Body          string    `json:"body"`
	RedactedPaths []string  `json:"redactedPaths"`
	Identity      *Identity `json:"identity"`
	Code          ErrorCode `json:"code"`
	Terminal      bool      `json:"terminal"`
}

// Suite reports whether this fixture is one the given suite must run.
func (f Fixture) Suite(name string) bool {
	if len(f.Suites) == 0 {
		return name == "client" // a bare merge fixture is the client's by default
	}
	for _, s := range f.Suites {
		if s == name {
			return true
		}
	}
	return false
}

// Corpus is the loaded conformance suite: the bodies (KRM objects) and the fixtures that use them.
type Corpus struct {
	Bodies   map[string]KRMObject
	Fixtures []Fixture
}

// Body returns a KRM object by its bodies/ reference. It is a hard error to miss: a fixture that
// names an object which does not exist is a broken contract, not a skippable test.
func (c Corpus) Body(ref string) (KRMObject, error) {
	obj, ok := c.Bodies[ref]
	if !ok {
		return nil, fmt.Errorf("conformance: no such body %q", ref)
	}
	return obj, nil
}

// Resolve turns a fixture event into the Event that must actually appear on the wire.
func (c Corpus) Resolve(scope *Scope, projection Projection, fe FixtureEvent) (Event, error) {
	ev := Event{Type: fe.Type}
	switch fe.Type {
	case EventReset:
		ev.Scope = scope
		ev.Projection = projection
		if scope != nil {
			ev.Target = scope.Target
		}
	case EventAdded, EventModified:
		obj, err := c.Body(fe.Body)
		if err != nil {
			return Event{}, err
		}
		ev.Object = obj
		// The protocol requires the array to be PRESENT, never merely optional — so a nil one
		// becomes empty here rather than vanishing from the JSON.
		ev.RedactedPaths = fe.RedactedPaths
		if ev.RedactedPaths == nil {
			ev.RedactedPaths = []string{}
		}
	case EventDeleted:
		ev.Identity = fe.Identity
	case EventError:
		ev.Code = fe.Code
		ev.Terminal = fe.Terminal
	case EventSynced:
	default:
		return Event{}, fmt.Errorf("conformance: unknown event type %q", fe.Type)
	}
	return ev, nil
}

// LoadCorpus reads the generated conformance JSON. dir is the repo's conformance/ directory;
// LoadConformance() finds it relative to this package.
func LoadCorpus(dir string) (Corpus, error) {
	var c Corpus
	if err := readJSON(filepath.Join(dir, "gen", "bodies.json"), &c.Bodies); err != nil {
		return c, err
	}
	if err := readJSON(filepath.Join(dir, "gen", "fixtures.json"), &c.Fixtures); err != nil {
		return c, err
	}
	return c, nil
}

// LoadConformance loads the corpus from the repository's conformance/ directory.
func LoadConformance() (Corpus, error) {
	return LoadCorpus(filepath.Join("..", "conformance"))
}

func readJSON(path string, into any) error {
	b, err := os.ReadFile(path) //nolint:gosec // a fixed, in-repo path
	if err != nil {
		return fmt.Errorf("conformance: %w (run `task fixtures`)", err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		return fmt.Errorf("conformance: %s: %w", path, err)
	}
	return nil
}
