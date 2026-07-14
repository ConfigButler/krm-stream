package gateway

import (
	"sort"
	"strings"
)

// Projection: what the stream removes, and what it redacts. Both are part of the WIRE (spec §3), not
// an implementation detail, because a consumer must be able to tell these three apart:
//
//	1. a field that does not exist upstream
//	2. a field the gateway REMOVED (never shown, never named)
//	3. a field that EXISTS but whose value was withheld — the path is named in redactedPaths
//
// Nothing about a VALUE distinguishes those, which is why redactedPaths is on the wire, per object,
// mandatory, and authoritative.
//
// # A redacted value is OMITTED. There is no placeholder, and that is the point (proposal 0003)
//
// This projection used to substitute a mask — `**REDACTED**` — for a Secret's value. That was the one
// poisoned value in the whole system, and we invented it: a browser holding `**REDACTED**` where a
// token should be can send it back on an ordinary save, and the literal string is written OVER the
// real secret. The token is destroyed, and from the browser it looked like a green tick.
//
// So we do not invent it. The value is simply not there, and `redactedPaths` says why — which is all
// the information the mask ever carried:
//
//   - the CONSUMER still knows the key exists and is withheld: `/data/token` is in redactedPaths. A
//     UI renders `••••••` from that, exactly as it did before.
//   - the DRAFT never contains a redacted value, so a merge patch cannot carry one back. The hazard
//     does not need a guard, because it can no longer arise.
//
// Keys-only disclosure survives intact. What does not survive is the mask-shaped landmine.

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

	// krm-editor/v1 additionally applies the Secret disclosure policy: keys-only.
	//
	// The value is DELETED, not masked. You learn that `token` exists — from redactedPaths, which names
	// `/data/token` — and you never learn or carry what it is.
	//
	// And when we empty a map, we REMOVE it, exactly as the annotations block above already does and
	// for exactly the same reason: **a `data: {}` that is empty only because WE emptied it is our
	// artifact, not the server's state.** Leaving it behind tells the consumer "this Secret has an
	// empty data map", which is a *different fact* from "this Secret's data is not yours to see" —
	// and the second is what happened. redactedPaths carries that, and it is the only thing that does.
	//
	// (An earlier version left `data: {}`, which contradicted the rule written four lines above it.
	// The reductio that settles it: under three verbs (proposal 0004), `ignore` on `status` must
	// obviously not leave `status: {}` behind. The same is true here, and it always was.)
	if p == ProjectionEditor && isSecret(out) {
		for _, field := range []string{"data", "stringData"} {
			m, ok := out[field].(map[string]any)
			if !ok {
				continue
			}
			redactedAny := false
			for k := range m {
				delete(m, k)
				redactedAny = true
				paths = append(paths, "/"+escapePointer(field)+"/"+escapePointer(k))
			}
			// Only when the emptiness is OURS. A Secret that genuinely arrived with `data: {}` keeps
			// it: we are removing what we removed, and nothing else.
			if redactedAny && len(m) == 0 {
				delete(out, field)
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
