package gateway_test

import (
	"encoding/json"
	"net/url"
	"os"
	"testing"

	"github.com/ConfigButler/krm-stream/gateway"
)

// The gateway's half of conformance/scopes.yaml — the REQUEST half of the wire.
//
// The client's suite reads the same table and asserts that resourceStreamURL() PRODUCES `canonical`.
// This one asserts that ScopeFromQuery() ACCEPTS it and yields `scope`. Between them the two ends of
// the repo cannot drift: the client's output is fed, byte for byte, into the server's parser.

type scopeCase struct {
	ID        string        `json:"id"`
	Because   string        `json:"because"`
	Query     string        `json:"query"`
	Canonical string        `json:"canonical"`
	Scope     gateway.Scope `json:"scope"`
	Code      string        `json:"code"`
}

func loadScopeCases(t *testing.T) []scopeCase {
	t.Helper()
	raw, err := os.ReadFile("../conformance/gen/scopes.json")
	if err != nil {
		t.Fatalf("read scopes corpus: %v (run `task fixtures`)", err)
	}
	var cases []scopeCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("parse scopes corpus: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("the scope corpus is empty")
	}
	return cases
}

func TestScopeFromQueryConformance(t *testing.T) {
	for _, c := range loadScopeCases(t) {
		t.Run(c.ID, func(t *testing.T) {
			q, err := url.ParseQuery(c.Query)
			if err != nil {
				t.Fatalf("the fixture's own query does not parse: %v", err)
			}

			got, serr := gateway.ScopeFromQuery(q)

			if c.Code != "" {
				if serr == nil {
					t.Fatalf("expected %s (%s), but the scope was ACCEPTED as %+v", c.Code, c.Because, got)
				}
				if string(serr.Code) != c.Code {
					t.Errorf("code = %s, want %s", serr.Code, c.Code)
				}
				// A rejection must be terminal: EventSource reconnects on its own, and would
				// otherwise hammer a scope that can never become valid.
				if !serr.Terminal {
					t.Error("a scope rejection must be TERMINAL, or the browser retries it forever")
				}
				return
			}

			if serr != nil {
				t.Fatalf("valid scope was rejected: %v (%s)", serr, c.Because)
			}
			if got != c.Scope {
				t.Errorf("scope = %+v, want %+v", got, c.Scope)
			}
		})
	}
}

// The seam, closed: what the CLIENT builds is what the GATEWAY parses. `canonical` is the client's
// output (its suite asserts that), and here it goes straight into the server's parser.
func TestCanonicalQueryRoundTrips(t *testing.T) {
	for _, c := range loadScopeCases(t) {
		if c.Code != "" {
			continue // a rejected scope has no canonical form
		}
		t.Run(c.ID, func(t *testing.T) {
			q, err := url.ParseQuery(c.Canonical)
			if err != nil {
				t.Fatalf("canonical query does not parse: %v", err)
			}
			got, serr := gateway.ScopeFromQuery(q)
			if serr != nil {
				t.Fatalf("the gateway REJECTED the query the client builds: %v", serr)
			}
			if got != c.Scope {
				t.Errorf("scope = %+v, want %+v", got, c.Scope)
			}

			// …and the Go twin of the builder agrees, so a host constructing a URL server-side (a
			// redirect, a link in a rendered page) produces the same bytes the client would.
			if enc := c.Scope.Query().Encode(); enc != mustCanonical(t, c.Canonical) {
				t.Errorf("Scope.Query() = %q, want %q", enc, mustCanonical(t, c.Canonical))
			}
		})
	}
}

// mustCanonical normalizes the fixture's canonical string the way url.Values.Encode() does (sorted
// keys, escaped), so the comparison is about CONTENT, not about which order a human typed it in.
func mustCanonical(t *testing.T, canonical string) string {
	t.Helper()
	q, err := url.ParseQuery(canonical)
	if err != nil {
		t.Fatalf("canonical query does not parse: %v", err)
	}
	return q.Encode()
}
