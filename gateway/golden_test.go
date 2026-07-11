package gateway

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The golden SSE transcripts: conformance/gen/sse/<id>.sse.
//
// This is the file that makes the contract real at the byte level. Until now the two suites shared
// JSON — the Go side asserted "I would emit these events", the TypeScript side asserted "given these
// events I hold this state" — and NOTHING checked that the events one side emits are the bytes the
// other side can read. Two implementations agreeing to disagree is exactly what that gap allows.
//
// So: the gateway writes each fixture through its REAL SSE sink, the bytes are committed, and the
// TypeScript suite parses those bytes back and replays them into the store. The wire is no longer a
// document both sides read; it is an artifact one side produces and the other consumes.
//
// Regenerate with `task fixtures` (or `go test -run TestSSEGoldens -update`). CI fails if stale,
// exactly as it does for conformance/gen/*.json — the goldens are generated, and a generated file
// that drifts from its generator is worse than no generated file.

var update = flag.Bool("update", false, "rewrite the golden SSE transcripts in conformance/gen/sse/")

func TestSSEGoldens(t *testing.T) {
	c := corpus(t)
	dir := filepath.Join("..", "conformance", "gen", "sse")
	if *update {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}

	for _, f := range c.Fixtures {
		if !f.Suite("gateway") || len(f.Watch) == 0 {
			continue
		}
		t.Run(f.ID, func(t *testing.T) {
			got := transcribe(t, c, f)
			path := filepath.Join(dir, f.ID+".sse")

			if *update {
				if err := os.WriteFile(path, got, 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}

			want, err := os.ReadFile(path) //nolint:gosec // a fixed, in-repo path
			if err != nil {
				t.Fatalf("%v\nrun `task fixtures` to generate the golden transcripts", err)
			}
			if !bytes.Equal(want, got) {
				t.Errorf("the wire has changed.\n--- want\n%s\n--- got\n%s", want, got)
			}
		})
	}
}

// transcribe replays a fixture through the gateway's real SSE sink and returns the bytes.
//
// A `disconnect` in the script means the browser's EventSource dropped and reconnected, so the
// transcript contains more than one HTTP response, concatenated — which is precisely what a consumer
// experiences across a reconnect, and precisely why the store must be idempotent about it.
//
// The `: connection N` line is the ONE thing here the gateway did not write. It is an SSE comment,
// which a conforming consumer ignores by definition (spec §7) — the same class of thing as a
// heartbeat — so putting it in the golden costs the contract nothing and buys the TypeScript parser a
// free proof that it really does ignore comments.
func transcribe(t *testing.T, c Corpus, f Fixture) []byte {
	t.Helper()
	var buf bytes.Buffer
	replay(t, c, f,
		func(conn int) Sink {
			fmt.Fprintf(&buf, ": connection %d\n\n", conn+1)
			return NewSSESink(&buf)
		},
		func(Sink) {},
	)
	return buf.Bytes()
}
