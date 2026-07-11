package gateway

import (
	"sort"
	"strings"
)

// Projection: what the stream removes, and what it masks. Both are part of the WIRE (spec §3), not
// an implementation detail, because a consumer must be able to tell these four apart:
//
//	1. a field that does not exist upstream
//	2. a field the gateway REMOVED (never shown)
//	3. a field that is present but REDACTED (shown as a placeholder)
//	4. a field whose real value merely LOOKS like a placeholder
//
// No amount of squinting at a value distinguishes 3 from 4 — which is why redactedPaths is on the
// wire, per object, mandatory, and why it is authoritative over anything the value looks like.

// RedactedPlaceholder is the mask a redacted value is replaced with. It is never the truth, and —
// this is the part that makes displaying a Secret safe at all — it can never be written back over
// the truth: the write path refuses any patch touching a path in redactedPaths (spec §3, gateway
// §7c). A real value that merely LOOKS like this is not redacted: redactedPaths is authoritative,
// and the shape of a value is never evidence.
const RedactedPlaceholder = "**REDACTED**"

// lastAppliedAnnotation is machinery a human editor must never see and must never round-trip.
const lastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// project applies the named projection to an object from the upstream, returning the object that
// goes on the wire and the RFC 6901 pointers whose values were masked in it.
//
// The returned object is always a DEEP COPY. The upstream's object may be shared (an informer's
// cache hands the same pointer to every subscriber, and the conformance corpus hands the same map to
// both sides of the assertion) — mutating it in place would corrupt state we do not own, and in the
// informer case would corrupt it for every other browser watching the same scope.
func project(p Projection, in KRMObject) (KRMObject, []string) {
	out := deepCopyObject(in)
	paths := []string{}

	// Both projections remove the machinery. There is no unspecified "other server-side
	// bookkeeping": if a gateway removes a field, that removal is part of a NAMED projection or it
	// does not happen.
	if meta, ok := out["metadata"].(map[string]any); ok {
		delete(meta, "managedFields")
		if ann, ok := meta["annotations"].(map[string]any); ok {
			delete(ann, lastAppliedAnnotation)
			// An annotations map that is empty ONLY because we emptied it is our artifact, not the
			// server's state. Leaving `annotations: {}` behind would tell the consumer the object
			// has an empty annotation map, which is a different fact from having none.
			if len(ann) == 0 {
				delete(meta, "annotations")
			}
		}
	}

	// krm-editor/v1 additionally applies the Secret disclosure policy: keys-only. You may see THAT
	// `token` exists — which is what makes the object editable at all, since you can still rename a
	// label on it — and you may never see or overwrite what it is.
	if p == ProjectionEditor && isSecret(out) {
		for _, field := range []string{"data", "stringData"} {
			m, ok := out[field].(map[string]any)
			if !ok {
				continue
			}
			for k := range m {
				m[k] = RedactedPlaceholder
				paths = append(paths, "/"+escapePointer(field)+"/"+escapePointer(k))
			}
		}
	}

	// Sorted so the wire is deterministic: two gateways given the same object emit the same bytes,
	// and a golden transcript is a golden transcript.
	sort.Strings(paths)
	return out, paths
}

func isSecret(o KRMObject) bool {
	kind, _ := o["kind"].(string)
	apiVersion, _ := o["apiVersion"].(string)
	return kind == "Secret" && apiVersion == "v1"
}

// escapePointer escapes one segment for RFC 6901: `~` becomes `~0`, `/` becomes `~1`. The order
// matters — escape the tilde FIRST, or the tilde you introduce while escaping a slash gets escaped
// again and `a/b` round-trips as `a~1b`.
func escapePointer(seg string) string {
	seg = strings.ReplaceAll(seg, "~", "~0")
	return strings.ReplaceAll(seg, "/", "~1")
}

// identityOf builds the tombstone identity for a deleted object, or nil if the object cannot supply
// a trustworthy uid.
//
// Nil is a real answer, not a failure to try: an informer's deletion tombstone can be degenerate,
// and a gateway that guesses a uid deletes the wrong object out of someone's browser. When this
// returns nil the caller begins a new snapshot cycle instead and lets reset…synced do the pruning
// (spec §4.2).
func identityOf(o KRMObject) *Identity {
	meta, ok := o["metadata"].(map[string]any)
	if !ok {
		return nil
	}
	uid, _ := meta["uid"].(string)
	name, _ := meta["name"].(string)
	if uid == "" || name == "" {
		return nil
	}
	ns, _ := meta["namespace"].(string)
	apiVersion, _ := o["apiVersion"].(string)
	kind, _ := o["kind"].(string)
	if apiVersion == "" || kind == "" {
		return nil
	}
	return &Identity{UID: uid, APIVersion: apiVersion, Kind: kind, Namespace: ns, Name: name}
}

// resourceVersionOf reads metadata.resourceVersion. It is opaque on the wire (spec §6) — a consumer
// must never parse or order by it — but the GATEWAY relies on it being a monotonic integer within
// one target, and that reliance is exactly what lets it promise per-object monotonicity to a
// consumer that cannot do the arithmetic itself.
func resourceVersionOf(o KRMObject) string {
	meta, ok := o["metadata"].(map[string]any)
	if !ok {
		return ""
	}
	rv, _ := meta["resourceVersion"].(string)
	return rv
}

func deepCopyObject(o KRMObject) KRMObject {
	return KRMObject(deepCopyValue(map[string]any(o)).(map[string]any))
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = deepCopyValue(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = deepCopyValue(x)
		}
		return out
	default:
		return v
	}
}
