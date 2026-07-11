// Command replay serves the conformance corpus over real HTTP, as a real SSE stream.
//
// It is a cluster you can point a browser at. Every fixture becomes an endpoint:
//
//	/resource-stream/v1?fixture=status-only-churn
//
// and connecting to it plays that scenario — the snapshot, the deltas, the mid-stream resync, the
// redacted Secret — through the real gateway, the real projection and the real SSE sink. Nothing is
// stubbed except the API server.
//
// Why it exists, given that the conformance suites already pass in-process: because "the client can
// read what the gateway wrote" and "the client can read what the gateway wrote OVER A NETWORK" are
// different claims. Chunk boundaries, flushing, Content-Type, heartbeats arriving in wall-clock time,
// and a terminal error actually CLOSING the connection are all invisible to an in-process test and
// all perfectly capable of breaking a browser.
//
//	go run ./cmd/replay --addr :8080
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"time"

	"github.com/ConfigButler/krm-stream/gateway"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "address to listen on")
	corpusDir := flag.String("corpus", filepath.Join("..", "conformance"), "path to the conformance/ directory")
	pace := flag.Duration("pace", 0, "default delay between events (override per request with ?pace=400ms)")
	static := flag.String("static", "", "directory to serve at / — the browser example")
	dist := flag.String("dist", "", "directory to serve at /krm-stream/ — the built, dependency-free ESM")
	flag.Parse()

	corpus, err := gateway.LoadCorpus(*corpusDir)
	if err != nil {
		log.Fatalf("replay: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/resource-stream/v1", stream(corpus, *pace))
	mux.HandleFunc("/fixtures", list(corpus))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprintln(w, "ok") })

	// The example and the library are served from the SAME ORIGIN as the stream, deliberately.
	//
	// Not for convenience: same-origin is the deployment the protocol is specified around (spec §7 —
	// native EventSource cannot send an Authorization header, so a v1 gateway MUST support same-origin
	// session cookies as its baseline). Serving the demo from a second port would mean CORS, which
	// would mean the demo proves something we do not ship.
	//
	// /krm-stream/ is the BUILT output, unbundled. The browser fetches index.js, which imports
	// ./store.js, which imports ./merge.js… If any of that needed a bundler, this page would be blank —
	// which makes the example the only real test of the constraint the whole library is designed around.
	if *dist != "" {
		mux.Handle("/krm-stream/", http.StripPrefix("/krm-stream/", http.FileServer(http.Dir(*dist))))
	}
	if *static != "" {
		mux.Handle("/", http.FileServer(http.Dir(*static)))
	}

	srv := &http.Server{
		Addr:    *addr,
		Handler: mux,
		// No WriteTimeout. An SSE stream is meant to stay open — a write deadline would sever a live
		// status watch on a schedule, which looks exactly like a bug in the client and is not.
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	log.Printf("replay: serving %d fixtures on http://%s", len(corpus.Fixtures), *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("replay: %v", err)
	}
}

func stream(corpus gateway.Corpus, pace time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("fixture")
		var f *gateway.Fixture
		for i := range corpus.Fixtures {
			if corpus.Fixtures[i].ID == id {
				f = &corpus.Fixtures[i]
			}
		}
		if f == nil || len(f.Watch) == 0 || f.Scope == nil {
			http.Error(w, fmt.Sprintf("no such replayable fixture: %q (see /fixtures)", id), http.StatusNotFound)
			return
		}

		// A `disconnect` op means the BROWSER's connection dropped, which on a real transport is not
		// something the server does — it is something that happens TO it. So a replay serves the ops
		// up to the first disconnect and then closes; the browser's own EventSource reconnects, and
		// gets the next segment. Which is, of course, exactly the scenario reconnect-prune describes.
		conn := connectionFor(r, f.Watch)
		backend, err := gateway.NewScriptedBackend(corpus, conn.ops)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// Per-request pacing. The automated suites want the stream as fast as the socket will carry it;
		// a human — and a browser test that needs to type into a field BEFORE the server changes it
		// underneath them — wants it slow. Same server, both.
		delay := pace
		if v := r.URL.Query().Get("pace"); v != "" {
			if d, err := time.ParseDuration(v); err == nil && d >= 0 && d < 10*time.Second {
				delay = d
			}
		}

		gw := &gateway.Gateway{
			Auth:       gateway.AllowAll{},
			Projection: f.Projection,
			Clients: func(string, gateway.Principal) (gateway.Backend, error) {
				return paced{backend, delay}, nil
			},
		}

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		go func() {
			// The script is finished: close the connection, the way an API server eventually does.
			// The consumer must survive it — and, if there is another segment, come back for it.
			<-backend.Exhausted()
			if conn.last {
				time.Sleep(50 * time.Millisecond) // let the final frame reach the socket
			}
			cancel()
		}()

		gw.ServeStream(w, r.WithContext(ctx), nil, *f.Scope)
	}
}

// connectionFor picks which segment of a multi-connection script this request gets. The browser tells
// us implicitly: EventSource reconnects, and the `connection` query param (which the demo page bumps)
// says which one it is on. A conformance fixture with no `disconnect` has exactly one.
type connection struct {
	ops  []gateway.WatchOp
	last bool
}

func connectionFor(r *http.Request, ops []gateway.WatchOp) connection {
	var conns [][]gateway.WatchOp
	cur := []gateway.WatchOp{}
	for _, op := range ops {
		if op.Op == "disconnect" {
			conns = append(conns, cur)
			cur = []gateway.WatchOp{}
			continue
		}
		cur = append(cur, op)
	}
	conns = append(conns, cur)

	n := 0
	if v := r.URL.Query().Get("connection"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n < 0 {
			n = 0
		}
	}
	if n >= len(conns) {
		n = len(conns) - 1
	}
	return connection{ops: conns[n], last: n == len(conns)-1}
}

// paced slows the script down so a human can watch it. At pace=0 it is a no-op and the stream arrives
// as fast as the socket takes it — which is what the automated e2e wants, and what no demo does.
type paced struct {
	inner gateway.Backend
	delay time.Duration
}

func (p paced) Watch(ctx context.Context, s gateway.Scope) (gateway.Watcher, error) {
	w, err := p.inner.Watch(ctx, s)
	if err != nil {
		return nil, err
	}
	return pacedWatcher{w, p.delay}, nil
}

type pacedWatcher struct {
	inner gateway.Watcher
	delay time.Duration
}

func (p pacedWatcher) Next(ctx context.Context) (gateway.WatchEvent, error) {
	if p.delay > 0 {
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			return gateway.WatchEvent{}, ctx.Err()
		}
	}
	return p.inner.Next(ctx)
}

func (p pacedWatcher) Stop() { p.inner.Stop() }

func list(corpus gateway.Corpus) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		var ids []string
		for _, f := range corpus.Fixtures {
			if len(f.Watch) > 0 {
				ids = append(ids, fmt.Sprintf("%-26s %s", f.ID, f.Title))
			}
		}
		sort.Strings(ids)
		for _, id := range ids {
			_, _ = fmt.Fprintln(w, id)
		}
	}
}
