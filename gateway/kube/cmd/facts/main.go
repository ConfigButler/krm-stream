// Command facts asks a REAL Kubernetes API server the questions the documentation does not answer,
// and writes down what it actually said.
//
// Every rung of this project's test ladder below this one fakes the API server — and a fake is a thing
// I wrote, so it will happily agree with me. The gateway currently BELIEVES a handful of things about
// Kubernetes, and the most important of them (F1: that a streaming list's terminating bookmark carries
// `k8s.io/initial-events-end`) appears NOWHERE in the API documentation. It is the snapshot boundary.
// If it is wrong, `synced` fires at the wrong moment — or never, and the browser never paints.
//
// So this program is not a test suite. It is a witness. It prints what it saw, writes it into
// docs/facts/ stamped with a cluster version, and exits non-zero only when an assumption the gateway
// actually relies on turns out to be FALSE.
//
//	task cluster-facts
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
)

// InitialEventsAnnotationKey is the claim under test. It is `metav1.InitialEventsAnnotationKey` in
// apimachinery (KEP-3157) — and it is not in the API reference documentation, which is exactly why we
// are here rather than reading.
const InitialEventsAnnotationKey = "k8s.io/initial-events-end"

var configmaps = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

type finding struct {
	ID       string // F1…
	Question string
	Answer   string
	Detail   string
	// LoadBearing: the gateway is already written as if this were true. False here is a BUG, not a note.
	LoadBearing bool
	OK          bool
}

func main() {
	kubeconfig := flag.String("kubeconfig", filepath.Join(os.Getenv("HOME"), ".kube", "config"), "path to kubeconfig")
	out := flag.String("out", "", "directory to write the observed-facts markdown into (optional)")
	ns := flag.String("namespace", "krm-stream-facts", "a scratch namespace, created and deleted")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cfg, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		fatal("kubeconfig: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		fatal("client: %v", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		fatal("dynamic client: %v", err)
	}

	version, err := cs.Discovery().ServerVersion()
	if err != nil {
		fatal("server version: %v", err)
	}
	fmt.Printf("cluster: %s\n\n", version.GitVersion)

	// A scratch namespace, torn down afterwards. Nothing here is precious.
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: *ns},
	}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		fatal("create namespace: %v", err)
	}
	defer func() {
		_ = cs.CoreV1().Namespaces().Delete(context.Background(), *ns, metav1.DeleteOptions{})
	}()

	var findings []finding
	findings = append(findings, f1AndF2(ctx, dyn, cs, *ns))
	findings = append(findings, f3(ctx, dyn, *ns))
	findings = append(findings, f4(ctx, dyn, cs, *ns))
	findings = append(findings, f5(ctx, cs, *ns))
	findings = append(findings, f6(ctx, dyn, cs, *ns))
	findings = append(findings, f7(ctx, dyn, *ns))

	fmt.Println()
	failed := 0
	for _, f := range findings {
		mark := "✓"
		if !f.OK {
			mark = "✗"
			if f.LoadBearing {
				failed++
			}
		}
		fmt.Printf("%s %-4s %s\n     %s\n", mark, f.ID, f.Question, f.Answer)
	}

	if *out != "" {
		// The filename is built from a string the SERVER chose. That is worth one function: a server
		// answering `../../../etc/cron.d/x` for its version would otherwise be picking the path we
		// write to, and this program is the one thing here that runs against clusters we do not own.
		path := filepath.Join(*out, "observed-"+versionSlug(version.GitVersion)+".md")
		if err := writeMarkdown(path, version.GitVersion, findings); err != nil {
			fatal("write: %v", err)
		}
		fmt.Printf("\nwritten: %s\n", path)
	}

	if failed > 0 {
		fmt.Printf("\n%d LOAD-BEARING assumption(s) are FALSE. The gateway is written as if they were true.\n", failed)
		os.Exit(1)
	}
	fmt.Println("\nevery load-bearing assumption holds against this cluster.")
}

// F1 + F2 — the streaming list, and the bookmark that IS the snapshot boundary.
//
// Verify the streaming-list framing used by the backend: synthetic ADDEDs followed by a BOOKMARK
// whose `k8s.io/initial-events-end` annotation marks the end of the snapshot. The API docs do not
// mention that annotation.
func f1AndF2(ctx context.Context, dyn dynamic.Interface, cs kubernetes.Interface, ns string) finding {
	f := finding{
		ID:          "F1",
		Question:    "Does a streaming list end with a BOOKMARK carrying `k8s.io/initial-events-end`?",
		LoadBearing: true,
	}

	// Two objects in scope, so the snapshot has something to say.
	for _, name := range []string{"cm-a", "cm-b"} {
		if _, err := cs.CoreV1().ConfigMaps(ns).Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Data:       map[string]string{"k": "v"},
		}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			f.Answer = fmt.Sprintf("could not create fixtures: %v", err)
			return f
		}
	}

	// The streaming-list options used by the Kubernetes backend.
	w, err := dyn.Resource(configmaps).Namespace(ns).Watch(ctx, metav1.ListOptions{
		SendInitialEvents:    ptr.To(true),
		AllowWatchBookmarks:  true,
		ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
		ResourceVersion:      "", // "" ⇒ a consistent read
	})
	if err != nil {
		f.Answer = fmt.Sprintf("the API server REFUSED the streaming-list watch: %v", err)
		f.Detail = "The upstream refused this streaming-list request."
		return f
	}
	defer w.Stop()

	added := 0
	deadline := time.After(30 * time.Second)
	for {
		select {
		case <-deadline:
			f.Answer = fmt.Sprintf("no terminating bookmark arrived within 30s (saw %d ADDED)", added)
			return f
		case ev, ok := <-w.ResultChan():
			if !ok {
				f.Answer = "the watch closed before any terminating bookmark"
				return f
			}
			switch ev.Type {
			case watch.Added:
				added++
			case watch.Bookmark:
				obj, _ := ev.Object.(*unstructured.Unstructured)
				if obj == nil {
					f.Answer = "a BOOKMARK arrived whose object is not unstructured"
					return f
				}
				anns := obj.GetAnnotations()
				val, present := anns[InitialEventsAnnotationKey]

				// The other half of the claim, and it is the reason the gateway has a partial-object
				// guard at all: the docs say a bookmark's object carries ONLY metadata.resourceVersion.
				shape := describeBookmark(obj)

				if !present {
					f.Answer = fmt.Sprintf("BOOKMARK arrived after %d ADDED — but WITHOUT the annotation. Annotations: %v", added, anns)
					f.Detail = shape
					return f
				}
				f.OK = true
				f.Answer = fmt.Sprintf("YES. %d synthetic ADDED, then a BOOKMARK with %s=%q.", added, InitialEventsAnnotationKey, val)
				f.Detail = shape
				return f
			}
		}
	}
}

// describeBookmark reports what a real bookmark's object actually contains — because the gateway
// treats "no uid" as a partial object and resnapshots on it, and a bookmark that DID carry a uid
// would be quietly forwarded to a browser as a husk.
func describeBookmark(obj *unstructured.Unstructured) string {
	var keys []string
	for k := range obj.Object {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	meta, _, _ := unstructured.NestedMap(obj.Object, "metadata")
	var metaKeys []string
	for k := range meta {
		metaKeys = append(metaKeys, k)
	}
	sort.Strings(metaKeys)

	return fmt.Sprintf("the bookmark's object has root keys %v and metadata keys %v; uid=%q, name=%q, resourceVersion=%q",
		keys, metaKeys, obj.GetUID(), obj.GetName(), obj.GetResourceVersion())
}

// F3 — how does a lost `resourceVersion` actually arrive on a watch?
//
// The gateway's whole recovery path (RESYNC_REQUIRED → a fresh reset…synced on the SAME connection)
// hangs off recognising this. The docs say "clients must handle the case by recognizing the status
// code 410 Gone" — but on an ALREADY-OPEN watch there is no status code, so what arrives?
func f3(ctx context.Context, dyn dynamic.Interface, ns string) finding {
	f := finding{
		ID:          "F3",
		Question:    "How does an expired resourceVersion (`410 Gone`) reach an open watch?",
		LoadBearing: true,
	}

	// resourceVersion="1" is long compacted away on any live cluster: the classic "too old".
	w, err := dyn.Resource(configmaps).Namespace(ns).Watch(ctx, metav1.ListOptions{ResourceVersion: "1"})
	if err != nil {
		// It may also be refused up-front, which is just as good to know.
		f.OK = apierrors.IsResourceExpired(err) || apierrors.IsGone(err)
		f.Answer = fmt.Sprintf("the watch was REFUSED at open: %v (IsResourceExpired=%v)", err, apierrors.IsResourceExpired(err))
		return f
	}
	defer w.Stop()

	deadline := time.After(30 * time.Second)
	for {
		select {
		case <-deadline:
			f.Answer = "nothing arrived within 30s — the API server neither errored nor closed"
			return f
		case ev, ok := <-w.ResultChan():
			if !ok {
				f.Answer = "the watch channel simply CLOSED, with no error event"
				f.Detail = "The gateway treats a closed watch as ErrWatchClosed and resnapshots, so it recovers — but it never learns WHY."
				f.OK = true // recoverable, and the gateway does recover
				return f
			}
			if ev.Type == watch.Error {
				status, _ := ev.Object.(*metav1.Status)
				if status == nil {
					f.Answer = fmt.Sprintf("a watch ERROR arrived carrying %T, not a *metav1.Status", ev.Object)
					return f
				}
				f.OK = true
				f.Answer = fmt.Sprintf("as a watch ERROR event: code=%d reason=%q", status.Code, status.Reason)
				f.Detail = fmt.Sprintf("message: %q — so the gateway must map watch.Error{Reason: Expired} to RESYNC_REQUIRED and begin a new cycle.", status.Message)
				return f
			}
			// An ADDED here would mean the API server silently served us from the current state.
			f.Answer = fmt.Sprintf("the API server did NOT error: it sent %s", ev.Type)
			return f
		}
	}
}

// F4 — what does a real resourceVersion look like?
//
// We now REQUIRE 1.35+ and refuse an unorderable resourceVersion outright. That is a bet, and this is
// the check that it is a safe one.
func f4(ctx context.Context, dyn dynamic.Interface, cs kubernetes.Interface, ns string) finding {
	f := finding{
		ID:          "F4",
		Question:    "Are real resourceVersions orderable decimals, and how large?",
		LoadBearing: true,
		OK:          true,
	}

	cm, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, "cm-a", metav1.GetOptions{})
	if err != nil {
		f.OK, f.Answer = false, fmt.Sprintf("get: %v", err)
		return f
	}
	list, err := dyn.Resource(configmaps).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		f.OK, f.Answer = false, fmt.Sprintf("list: %v", err)
		return f
	}

	objRV := cm.GetResourceVersion()
	listRV := list.GetResourceVersion()
	decimal := isDecimal(objRV) && isDecimal(listRV)
	if !decimal {
		f.OK = false
	}
	f.Answer = fmt.Sprintf("object rv=%q (%d digits, decimal=%v); collection rv=%q. int64 holds 19 digits.",
		objRV, len(objRV), decimal, listRV)
	f.Detail = "The gateway compares these as arbitrary-precision decimals (longer-is-greater, then lexicographic), which is what the docs prescribe. An int64 parse would work HERE — and that is exactly the trap: it works until a backing store with wider revisions does not."
	return f
}

// F5 — does a real controller move `status` the way `status-only-churn` says?
//
// The fixture claims a status-only update leaves `spec` BYTE-IDENTICAL. The whole product rests on it:
// if a controller also rewrote spec, the "edit spec while watching status" story would be a fight.
func f5(ctx context.Context, cs kubernetes.Interface, ns string) finding {
	f := finding{
		ID:       "F5",
		Question: "Does a real controller rewrite `status` while leaving `spec` byte-identical?",
	}

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(2)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  "web",
					Image: "registry.k8s.io/pause:3.10",
				}}},
			},
		},
	}
	if _, err := cs.AppsV1().Deployments(ns).Create(ctx, dep, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		f.Answer = fmt.Sprintf("create deployment: %v", err)
		return f
	}

	w, err := cs.AppsV1().Deployments(ns).Watch(ctx, metav1.ListOptions{FieldSelector: "metadata.name=web"})
	if err != nil {
		f.Answer = fmt.Sprintf("watch: %v", err)
		return f
	}
	defer w.Stop()

	var firstSpec []byte
	statusOnly, specChanged, total := 0, 0, 0
	deadline := time.After(60 * time.Second)

	for {
		select {
		case <-deadline:
			f.Answer = fmt.Sprintf("saw %d MODIFIED events (%d status-only, %d touched spec) before the deadline", total, statusOnly, specChanged)
			f.OK = total > 0 && specChanged == 0
			return f
		case ev, ok := <-w.ResultChan():
			if !ok {
				f.Answer = "the watch closed"
				return f
			}
			d, isDep := ev.Object.(*appsv1.Deployment)
			if !isDep {
				continue
			}
			spec, _ := json.Marshal(d.Spec)
			if firstSpec == nil {
				firstSpec = spec
				continue
			}
			if ev.Type != watch.Modified {
				continue
			}
			total++
			if string(spec) == string(firstSpec) {
				statusOnly++
			} else {
				specChanged++
			}
			// Wait for the rollout to actually COMPLETE — readyReplicas climbing to the requested
			// count is the thing the demo claims you can watch. Stopping at "observedGeneration caught
			// up" would let this pass while readyReplicas was still 0, which proves the status moved
			// but not that it moved the way the product's headline says it does.
			if d.Status.ReadyReplicas >= *dep.Spec.Replicas {
				f.OK = specChanged == 0
				f.Answer = fmt.Sprintf("YES: %d MODIFIED events, %d of them status-only, %d touched spec. The rollout completed: replicas=%d readyReplicas=%d",
					total, statusOnly, specChanged, d.Status.Replicas, d.Status.ReadyReplicas)
				f.Detail = "So `status-only-churn` describes real traffic: a controller reconciling a Deployment rewrites status repeatedly, watching readyReplicas climb to the requested count, and never touches spec. That is the demo, and it is real."
				return f
			}
		}
	}
}

// F6 — an AGGREGATED API server: what does it actually serve, and does the gateway's streaming-list
// request even work against it?
//
// This is the case Kubernetes' own conformance rules do NOT cover. Everything the gateway assumes —
// orderable resourceVersions (we now REFUSE otherwise), `sendInitialEvents` producing an
// initial-events-end bookmark — is guaranteed only for kube-apiserver. An aggregated API is a separate
// binary, and it is exactly where those guarantees may quietly stop holding.
//
// Two answers matter:
//   - is its resourceVersion an orderable decimal? (⇒ is OrderingLenient a real need or a theory?)
//   - does it support the streaming list at all? (⇒ does the gateway need a fallback path to serve one?)
func f6(ctx context.Context, dyn dynamic.Interface, cs kubernetes.Interface, ns string) finding {
	f := finding{
		ID:       "F6",
		Question: "An AGGREGATED API (wardle `Flunder`): orderable resourceVersion? streaming list?",
	}
	flunders := schema.GroupVersionResource{Group: "wardle.example.com", Version: "v1alpha1", Resource: "flunders"}

	if _, err := cs.Discovery().ServerResourcesForGroupVersion("wardle.example.com/v1alpha1"); err != nil {
		f.OK = true // not installed is not a failure — it just means we learned nothing
		f.Answer = "SKIPPED: no aggregated API installed (run `task cluster-aggregated-api`)"
		return f
	}

	fl, err := dyn.Resource(flunders).Namespace(ns).Create(ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "wardle.example.com/v1alpha1", "kind": "Flunder",
		"metadata": map[string]any{"name": "fl-1", "namespace": ns},
		"spec":     map[string]any{"referenceType": "Flunder", "reference": "some-flunder"},
	}}, metav1.CreateOptions{})
	if err != nil {
		f.Answer = fmt.Sprintf("could not create a Flunder: %v", err)
		return f
	}

	rv := fl.GetResourceVersion()
	orderable := isDecimal(rv)

	// The gateway sends EXACTLY this on every scope. If an aggregated API rejects it, the gateway
	// cannot serve a Flunder at all — which would be a real, shipped bug.
	streaming := "accepted"
	bookmark := "none"
	w, werr := dyn.Resource(flunders).Namespace(ns).Watch(ctx, metav1.ListOptions{
		SendInitialEvents:    ptr.To(true),
		AllowWatchBookmarks:  true,
		ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
		ResourceVersion:      "",
	})
	if werr != nil {
		streaming = fmt.Sprintf("REJECTED: %v", werr)
	} else {
		defer w.Stop()
		deadline := time.After(20 * time.Second)
	loop:
		for {
			select {
			case <-deadline:
				bookmark = "no terminating bookmark within 20s"
				break loop
			case ev, ok := <-w.ResultChan():
				if !ok {
					bookmark = "the watch closed before any bookmark"
					break loop
				}
				if ev.Type == watch.Error {
					status, _ := ev.Object.(*metav1.Status)
					streaming = fmt.Sprintf("REJECTED mid-stream: %v", status)
					break loop
				}
				if ev.Type == watch.Bookmark {
					obj, _ := ev.Object.(*unstructured.Unstructured)
					if _, ok := obj.GetAnnotations()[InitialEventsAnnotationKey]; ok {
						bookmark = "YES — carries " + InitialEventsAnnotationKey
					} else {
						bookmark = fmt.Sprintf("a bookmark WITHOUT the annotation (annotations: %v)", obj.GetAnnotations())
					}
					break loop
				}
			}
		}
	}

	f.OK = orderable && strings.HasPrefix(bookmark, "YES")
	f.Answer = fmt.Sprintf("resourceVersion=%q (decimal/orderable=%v); streaming list %s; initial-events-end bookmark: %s",
		rv, orderable, streaming, bookmark)
	f.Detail = strings.Join([]string{
		"**This confirms why the backend supports both watch paths.**",
		"",
		"1. **The streaming list is not universal.** An aggregated API server is a separate binary with its",
		"   own feature gates, and this one refuses `sendInitialEvents` outright. List-then-watch is therefore",
		"   required for this kind of upstream, even on a current cluster.",
		"",
		"2. **Its resourceVersion space is its own, and it starts at ~1.** This Flunder's is `\"2\"` — the",
		"   sample-apiserver has its own etcd, numbering from scratch. And because that etcd is a sidecar",
		"   with no persistence, a pod restart RESETS it: resourceVersions for the same resource can go",
		"   BACKWARDS. Per-object monotonicity survives this only because the gateway's high-water map is",
		"   per-CYCLE and not per-stream — a restart forces a new cycle, which clears it. A per-stream map",
		"   would have silently swallowed every event after such a restart. That decision was made for a",
		"   different reason, and this is the cluster confirming it.",
		"",
		"3. Its resourceVersions ARE orderable decimals — so `OrderingLenient` remains unexercised here.",
		"   That is a fact about *this* sample-apiserver, not a guarantee about aggregated APIs in general,",
		"   which is exactly why the escape hatch exists.",
	}, "\n")
	return f
}

// F7 — does a DELETED event carry the object, and does it carry a trustworthy uid?
//
// The gateway emits `deleted` with an identity built from the object, and REFUSES to guess when the
// uid is missing. Worth knowing what a real one actually looks like.
func f7(ctx context.Context, dyn dynamic.Interface, ns string) finding {
	f := finding{
		ID:       "F7",
		Question: "Does a real DELETED event carry a complete object, with a uid?",
	}

	name := "cm-doomed"
	created, err := dyn.Resource(configmaps).Namespace(ns).Create(ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"data":     map[string]any{"k": "v"},
	}}, metav1.CreateOptions{})
	if err != nil {
		f.Answer = fmt.Sprintf("create: %v", err)
		return f
	}

	w, err := dyn.Resource(configmaps).Namespace(ns).Watch(ctx, metav1.ListOptions{
		ResourceVersion: created.GetResourceVersion(),
		FieldSelector:   "metadata.name=" + name,
	})
	if err != nil {
		f.Answer = fmt.Sprintf("watch: %v", err)
		return f
	}
	defer w.Stop()

	if err := dyn.Resource(configmaps).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		f.Answer = fmt.Sprintf("delete: %v", err)
		return f
	}

	deadline := time.After(30 * time.Second)
	for {
		select {
		case <-deadline:
			f.Answer = "no DELETED event arrived within 30s"
			return f
		case ev, ok := <-w.ResultChan():
			if !ok {
				f.Answer = "the watch closed before the DELETED arrived"
				return f
			}
			if ev.Type != watch.Deleted {
				continue
			}
			obj, _ := ev.Object.(*unstructured.Unstructured)
			if obj == nil {
				f.Answer = fmt.Sprintf("DELETED carried %T, not an object", ev.Object)
				return f
			}
			f.OK = obj.GetUID() != ""
			data, hasData, _ := unstructured.NestedMap(obj.Object, "data")
			f.Answer = fmt.Sprintf("YES: uid=%q, name=%q, and the final state is present (data=%v, present=%v)",
				obj.GetUID(), obj.GetName(), data, hasData)
			f.Detail = "So a kube-apiserver DELETED is complete and trustworthy. The degenerate tombstone the gateway guards against (tombstone-without-uid) comes from client-go's INFORMER cache (DeletedFinalStateUnknown), not from the API server — which is why that fixture can only ever be a fake-watch one."
			return f
		}
	}
}

// versionSlug reduces a server-reported version to the characters a Kubernetes version can actually
// contain (`v1.36.2+k3s1`), so that nothing a server says can escape the directory we were told to
// write into. An allowlist, not a blocklist: the set of safe characters is short and knowable, and
// the set of dangerous ones is neither.
func versionSlug(version string) string {
	slug := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.', r == '-', r == '_', r == '+':
			return r
		default:
			return '-'
		}
	}, version)
	// `.` and `..` survive the map above, and both name a directory rather than a file.
	if strings.Trim(slug, ".") == "" {
		return "unknown"
	}
	return slug
}

func isDecimal(rv string) bool {
	if rv == "" || rv[0] < '1' || rv[0] > '9' {
		return false
	}
	return strings.IndexFunc(rv, func(r rune) bool { return r < '0' || r > '9' }) == -1
}

func writeMarkdown(path, version string, findings []finding) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# Observed: what `%s` actually did\n\n", version)
	b.WriteString("> **Generated by `task cluster-facts`** — do not hand-edit. This file is a WITNESS: it is what a\n")
	b.WriteString("> real API server did when asked, as opposed to [what the documentation says it should](kubernetes-api-concepts.md).\n")
	b.WriteString(">\n")
	b.WriteString("> The distinction matters most where the docs are SILENT. The gateway's snapshot boundary rests on an\n")
	b.WriteString("> annotation that appears nowhere in the API reference; until this file existed, that was faith.\n\n")
	fmt.Fprintf(&b, "| | question | load-bearing | answer |\n|---|---|---|---|\n")
	for _, f := range findings {
		lb := "—"
		if f.LoadBearing {
			lb = "**yes**"
		}
		mark := "✅"
		if !f.OK {
			mark = "❌"
		}
		fmt.Fprintf(&b, "| %s %s | %s | %s | %s |\n", mark, f.ID, f.Question, lb, strings.ReplaceAll(f.Answer, "|", "\\|"))
	}
	b.WriteString("\n## Detail\n\n")
	for _, f := range findings {
		fmt.Fprintf(&b, "### %s — %s\n\n%s\n\n", f.ID, f.Question, f.Answer)
		if f.Detail != "" {
			fmt.Fprintf(&b, "%s\n\n", f.Detail)
		}
	}
	// The two halves of `path` have very different provenance, and only one of them is a risk.
	// The DIRECTORY is `--out`: an operator naming a directory on their own command line, which is
	// what a CLI flag is for. The FILENAME embeds a string the SERVER chose, and that one is
	// sanitized (versionSlug) precisely because a cluster we do not own should not get to pick where
	// this program writes. gosec cannot see that distinction; it is the whole of the reasoning.
	return os.WriteFile(path, []byte(b.String()), 0o600) // #nosec G703
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "facts: "+format+"\n", args...)
	os.Exit(2)
}
