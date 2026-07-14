// Package kube is the Kubernetes adapter: the only code in this repo that knows what an API server
// is, and the only module that imports client-go.
//
// The gateway core is a protocol — reset … added* … synced, then live deltas — and it reaches its
// upstream through one interface (gateway.Backend). This package is the implementation of that
// interface for a real Kubernetes API server, and it is a SEPARATE MODULE so that
// `go get .../krm-stream/gateway` stays dependency-free. Adopting the protocol should not mean
// adopting client-go and its transitive world.
//
// # Two paths, and both are required
//
// The obvious reading of the Kubernetes docs is that a modern cluster gives you a streaming list
// (§3a) and that list-then-watch (§3b) is a compatibility shim for old ones. A real cluster says
// otherwise. `task cluster-facts` (F6) pointed a §3a request at Kubernetes' own sample-apiserver —
// an ordinary aggregated API on a current cluster — and it was refused outright:
//
//	ListOptions.meta.k8s.io "" is invalid: sendInitialEvents: Forbidden:
//	  sendInitialEvents is forbidden for watch unless the WatchList feature gate is enabled
//
// An aggregated API server is a separate binary with its own feature gates; WatchList being on in
// kube-apiserver says nothing about it. So a backend that implements only §3a cannot open a stream
// for an aggregated resource AT ALL — and it would have failed in a user's cluster, not in our
// tests. Both paths ship, and the choice between them is DETECTED rather than configured: nobody
// should have to know which of their APIs is aggregated in order to watch it.
//
// What the two paths have in common is the only thing the gateway cares about: the snapshot arrives
// as WatchAdded events terminated by a bookmark whose InitialEventsEnd is set, and everything after
// that bookmark is live. On the §3a path the API server hands us that boundary. On the §3b path we
// synthesize it. That is precisely why the protocol names the BOUNDARY and not the mechanism.
//
// # The failure mode this does NOT defend against, and why
//
// A server could ACCEPT `sendInitialEvents` and then quietly ignore it — no synthetic ADDEDs, no
// terminating bookmark, so `synced` never fires and a browser never paints. We do not guard against
// that, and the omission is deliberate: the only possible guard is a timeout ("no bookmark in N
// seconds ⇒ assume §3b"), and N would be a guess that turns a slow cluster into a corrupt one. What
// we have instead is a stated environment: this gateway requires Kubernetes 1.35+ (README §3), where
// the option is not silently droppable. A server that accepts an option and ignores it is broken in
// a way that is not ours to paper over — and the honest response to a broken upstream is to be
// diagnosable, not to guess.
package kube

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/utils/ptr"

	"github.com/ConfigButler/krm-stream/gateway"
)

// initialEventsEndAnnotation marks the bookmark that closes a streaming list's snapshot. It is
// metav1.InitialEventsAnnotationKey in apimachinery (KEP-3157), and it appears NOWHERE in the API
// reference documentation — which is why we did not take it on faith. A real v1.36.2 API server
// really does send it (F1), and this gateway's entire `synced` boundary rests on that one fact.
const initialEventsEndAnnotation = metav1.InitialEventsAnnotationKey

// Backend is a real Kubernetes API server, behind the gateway's Backend seam.
//
// One Backend is one upstream target, reached as one principal — the host builds it in its
// ClientFor and is therefore the only thing that ever sees a kubeconfig, a token or an
// impersonation header. The gateway never does.
type Backend struct {
	client dynamic.Interface

	// mu guards listThenWatch, which is a per-GroupVersion memory of "this API refused the
	// streaming list". It is a cache of a fact about the SERVER, not about a request, so it is
	// shared across every stream this Backend serves.
	mu            sync.Mutex
	listThenWatch map[schema.GroupVersion]bool
}

// NewBackend wraps a dynamic client as a gateway upstream.
func NewBackend(client dynamic.Interface) *Backend {
	return &Backend{
		client:        client,
		listThenWatch: map[schema.GroupVersion]bool{},
	}
}

var _ gateway.Backend = (*Backend)(nil)

// Watch opens a snapshot-then-live stream for one scope.
//
// It tries the streaming list first, and falls back — permanently, for that API's GroupVersion — to
// list-then-watch when the server refuses `sendInitialEvents`. The fallback is remembered because
// the refusal is a property of the API server binary, not of this request: re-asking on every
// snapshot cycle would spend a doomed round-trip per cycle, forever, on exactly the aggregated APIs
// that are already the slowest to reach.
func (b *Backend) Watch(ctx context.Context, scope gateway.Scope) (gateway.Watcher, error) {
	gv := schema.GroupVersion{Group: scope.Group, Version: scope.Version}
	ri := b.resourceFor(scope)

	if b.prefersListThenWatch(gv) {
		return b.listThenWatchStream(ctx, ri, scope)
	}

	w, err := ri.Watch(ctx, streamingListOptions(scope))
	if err == nil {
		return &channelWatcher{w: w}, nil
	}
	if !isSendInitialEventsRefused(err) {
		// Anything else is a real error — a 403, a nonexistent resource, a dead API server — and it
		// is NOT ours to paper over. Falling back on every failure would turn "you may not watch
		// Secrets" into a second, differently-worded permission denial, which is how a gateway ends
		// up lying about why it could not open a stream.
		return nil, fmt.Errorf("krm-stream/kube: streaming list for %s: %w", scope.Resource, err)
	}

	// F6, in production. This API is aggregated (or otherwise has WatchList off); §3b is not a
	// fallback here, it is the only way in.
	b.rememberListThenWatch(gv)
	return b.listThenWatchStream(ctx, ri, scope)
}

// streamingListOptions is §3a, and it is exactly the request the fact-finder verified against a real
// API server. ResourceVersion: "" means "the freshest state" — a consistent read — and is what makes
// the snapshot a snapshot rather than a replay from a stale point.
func streamingListOptions(scope gateway.Scope) metav1.ListOptions {
	o := selectors(scope)
	o.AllowWatchBookmarks = true
	o.SendInitialEvents = ptr.To(true)
	o.ResourceVersionMatch = metav1.ResourceVersionMatchNotOlderThan
	o.ResourceVersion = ""
	return o
}

// selectors turns a scope into the server-side filters that make it that scope. A named scope is a
// field selector, not a Get: it must arrive on the SAME watch as everything else, so that a single
// stream can carry it and the gateway needs no second code path for "one object".
func selectors(scope gateway.Scope) metav1.ListOptions {
	o := metav1.ListOptions{LabelSelector: scope.LabelSelector}
	if scope.Name != "" {
		o.FieldSelector = "metadata.name=" + scope.Name
	}
	return o
}

func (b *Backend) resourceFor(scope gateway.Scope) dynamic.ResourceInterface {
	gvr := schema.GroupVersionResource{
		Group:    scope.Group,
		Version:  scope.Version,
		Resource: scope.Resource,
	}
	if scope.Namespace == "" {
		// Cluster-scoped, or "across all namespaces" — the host's Authorizer has already decided the
		// caller may see that, and it is not this code's business to second-guess it.
		return b.client.Resource(gvr)
	}
	return b.client.Resource(gvr).Namespace(scope.Namespace)
}

func (b *Backend) prefersListThenWatch(gv schema.GroupVersion) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.listThenWatch[gv]
}

func (b *Backend) rememberListThenWatch(gv schema.GroupVersion) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.listThenWatch[gv] = true
}

// listThenWatchStream is §3b: list at a resourceVersion, then watch from exactly there.
//
// The gap that everyone worries about does not exist, and the reason is worth stating: the watch is
// opened at the LIST's resourceVersion, so the API server replays anything that happened in
// between. Nothing is lost by the seam between the two calls — which is why the watch may be opened
// after the list is read, and why holding the list in memory is the only real cost.
func (b *Backend) listThenWatchStream(ctx context.Context, ri dynamic.ResourceInterface, scope gateway.Scope) (gateway.Watcher, error) {
	list, err := ri.List(ctx, selectors(scope))
	if err != nil {
		return nil, fmt.Errorf("krm-stream/kube: list %s: %w", scope.Resource, err)
	}

	o := selectors(scope)
	o.AllowWatchBookmarks = true
	o.ResourceVersion = list.GetResourceVersion() // resume EXACTLY where the list ended: no gap.
	w, err := ri.Watch(ctx, o)
	if err != nil {
		return nil, fmt.Errorf("krm-stream/kube: watch %s from resourceVersion %q: %w",
			scope.Resource, list.GetResourceVersion(), err)
	}

	// The snapshot, in the shape §3a would have delivered it — including the boundary bookmark,
	// which here is OURS to synthesize because the API server would not. The gateway cannot tell the
	// difference, and that is the entire point of the seam.
	snapshot := make([]gateway.WatchEvent, 0, len(list.Items)+1)
	for i := range list.Items {
		snapshot = append(snapshot, gateway.WatchEvent{
			Type:   gateway.WatchAdded,
			Object: gateway.KRMObject(list.Items[i].Object),
		})
	}
	snapshot = append(snapshot, gateway.WatchEvent{
		Type:             gateway.WatchBookmark,
		InitialEventsEnd: true,
	})

	return &prologueWatcher{queue: snapshot, live: &channelWatcher{w: w}}, nil
}

// isSendInitialEventsRefused recognises the one refusal that means "this server cannot do §3a".
//
// Observed on a real aggregated API (F6) as a 422 Invalid naming the `sendInitialEvents` field. We
// are liberal about the status code — a different server may say 400 or 403, and the docs promise
// nothing here — and STRICT about the field, because that string is the whole discriminator between
// "your cluster cannot stream lists" and "you may not watch this at all".
func isSendInitialEventsRefused(err error) bool {
	if err == nil {
		return false
	}
	if !apierrors.IsInvalid(err) && !apierrors.IsBadRequest(err) && !apierrors.IsForbidden(err) {
		return false
	}

	// The structured form first: a *StatusError whose causes name the field.
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		if details := status.Status().Details; details != nil {
			for _, cause := range details.Causes {
				if strings.Contains(cause.Field, "sendInitialEvents") ||
					strings.Contains(cause.Message, "sendInitialEvents") {
					return true
				}
			}
		}
	}
	// …and the message, for a server that reports it less carefully.
	return strings.Contains(err.Error(), "sendInitialEvents")
}

// channelWatcher adapts client-go's PUSH watch (a channel) to the gateway's PULL Watcher.
//
// This is the ten lines seams.go promised, and the direction matters: Next() returning is the
// gateway's proof that it finished with the previous event, which is what makes coalescing and the
// conformance replay deterministic instead of racy. Recovering that synchronisation point from a
// channel afterwards is not possible at all — so the adapter goes this way, and only this way.
type channelWatcher struct {
	w watch.Interface
}

func (c *channelWatcher) Next(ctx context.Context) (gateway.WatchEvent, error) {
	select {
	case <-ctx.Done():
		return gateway.WatchEvent{}, ctx.Err()
	case ev, ok := <-c.w.ResultChan():
		if !ok {
			// A clean close. An API server times a watch out routinely, so this is not an error —
			// it means "reopen", and the gateway reopens with a FRESH snapshot cycle because it can
			// no longer promise it saw everything in between.
			return gateway.WatchEvent{}, gateway.ErrWatchClosed
		}
		return translate(ev)
	}
}

func (c *channelWatcher) Stop() { c.w.Stop() }

// prologueWatcher plays a queue of events (the synthesized snapshot) and then delegates to the live
// watch. It exists so that §3b's caller — the stream loop — sees exactly the §3a event sequence.
type prologueWatcher struct {
	queue []gateway.WatchEvent
	live  gateway.Watcher
}

func (p *prologueWatcher) Next(ctx context.Context) (gateway.WatchEvent, error) {
	if err := ctx.Err(); err != nil {
		return gateway.WatchEvent{}, err
	}
	if len(p.queue) > 0 {
		ev := p.queue[0]
		p.queue = p.queue[1:]
		return ev, nil
	}
	return p.live.Next(ctx)
}

func (p *prologueWatcher) Stop() { p.live.Stop() }

// translate maps Kubernetes's watch vocabulary onto the gateway's. The translation is lossy on
// purpose: BOOKMARK and ERROR never reach a browser (spec §2), and the gateway is where they stop.
func translate(ev watch.Event) (gateway.WatchEvent, error) {
	if ev.Type == watch.Error {
		// How a 410 Gone actually arrives on an OPEN watch (F3): not as a status code — there is no
		// response left to put one on — but as an event carrying a *metav1.Status. The gateway turns
		// any upstream error into a new snapshot cycle, so it recovers either way; naming it is what
		// makes the log say something true.
		return gateway.WatchEvent{Type: gateway.WatchError, Err: watchError(ev.Object)}, nil
	}

	obj, ok := ev.Object.(*unstructured.Unstructured)
	if !ok {
		// The dynamic client deals in unstructured. Anything else is a bug in our wiring, not a
		// condition to recover from — and a gateway that silently dropped it would be inventing a
		// gap in a stream it promised was gap-free.
		return gateway.WatchEvent{}, fmt.Errorf("krm-stream/kube: %s event carried %T, not an object", ev.Type, ev.Object)
	}

	switch ev.Type {
	case watch.Added:
		return gateway.WatchEvent{Type: gateway.WatchAdded, Object: gateway.KRMObject(obj.Object)}, nil
	case watch.Modified:
		return gateway.WatchEvent{Type: gateway.WatchModified, Object: gateway.KRMObject(obj.Object)}, nil
	case watch.Deleted:
		return gateway.WatchEvent{Type: gateway.WatchDeleted, Object: gateway.KRMObject(obj.Object)}, nil
	case watch.Bookmark:
		// THE snapshot boundary — or a routine bookmark, which the gateway absorbs for its
		// resourceVersion and never forwards. The annotation is the only thing that distinguishes
		// them, and a real bookmark carries no uid (F1), which is what the gateway's partial-object
		// guard keys on: a bookmark that leaked through would BLANK an object in someone's browser.
		return gateway.WatchEvent{
			Type:             gateway.WatchBookmark,
			Object:           gateway.KRMObject(obj.Object),
			InitialEventsEnd: obj.GetAnnotations()[initialEventsEndAnnotation] == "true",
		}, nil
	case watch.Error:
		// Handled above; here to keep the switch exhaustive.
		return gateway.WatchEvent{Type: gateway.WatchError, Err: watchError(ev.Object)}, nil
	default:
		return gateway.WatchEvent{}, fmt.Errorf("krm-stream/kube: unknown watch event type %q", ev.Type)
	}
}

// watchError renders the *metav1.Status on a watch.Error as an error the gateway can act on. A 410
// Gone / "Expired" is the continuity-losing one, and it is not fatal: it means "start a new snapshot
// cycle", which is exactly what a non-terminal RESYNC_REQUIRED tells the consumer.
func watchError(obj runtime.Object) error {
	if status, ok := obj.(*metav1.Status); ok {
		if status.Reason == metav1.StatusReasonExpired || status.Code == 410 {
			return gateway.ResyncRequired(fmt.Sprintf(
				"the upstream resourceVersion expired (410 Gone): %s", status.Message))
		}
		return gateway.ResyncRequired(fmt.Sprintf("upstream watch error: %s (reason=%s code=%d)",
			status.Message, status.Reason, status.Code))
	}
	return gateway.ResyncRequired(fmt.Sprintf("upstream watch error carrying %T", obj))
}
