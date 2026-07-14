package gateway

import (
	"encoding/json"
	"fmt"
)

// PatchViolation is a merge-patch change that would write a path the stream did not disclose.
// A host should return it as a client error and never send the patch to Kubernetes.
type PatchViolation struct {
	Path   string
	Reason string
}

func (e *PatchViolation) Error() string {
	return fmt.Sprintf("krm-stream: patch must not touch %s: %s", e.Path, e.Reason)
}

// ValidateMergePatch checks a host-owned RFC 7386 JSON merge patch before that host writes it.
//
// krm-stream deliberately has no write endpoint: authentication, authorization, audit, and the
// Kubernetes client all remain the host's responsibility. This helper closes the easy-to-miss gap at
// that boundary by refusing patches that touch a value redacted by the effective projection, a path
// removed by that projection, or a whole subtree that contains either.
//
// object must be the current server object from the same scope and projection decision used for the
// stream. The function sends nothing and does not mutate object or patch.
func ValidateMergePatch(projection Projection, object KRMObject, patch []byte) error {
	if !isBuiltinProjection(projection) {
		return fmt.Errorf("krm-stream: unknown projection %q", projection)
	}

	var parsed map[string]any
	if err := json.Unmarshal(patch, &parsed); err != nil {
		return fmt.Errorf("krm-stream: invalid JSON merge patch: %w", err)
	}
	if parsed == nil {
		return fmt.Errorf("krm-stream: JSON merge patch must be an object")
	}

	protected := projectionProtectedPaths(projection, object)
	return validatePatchObject(parsed, nil, protected)
}

func projectionProtectedPaths(projection Projection, object KRMObject) map[string]string {
	_, redacted := project(projection, object)
	paths := make(map[string]string, len(redacted)+3)
	for _, value := range redacted {
		paths[value.path] = "redacted by the effective projection"
	}

	// These paths are projection policy, not a property of this particular object. Rejecting an
	// attempt to add one back is as important as rejecting an attempt to modify one that existed.
	paths["/metadata/managedFields"] = "removed by the projection"
	paths["/metadata/annotations/"+escapePointer(lastAppliedAnnotation)] = "removed by the projection"
	if projection == ProjectionSpec {
		paths["/status"] = "ignored by the projection"
	}
	return paths
}

func validatePatchObject(patch map[string]any, path []string, protected map[string]string) error {
	for key, value := range patch {
		next := append(append([]string(nil), path...), key)
		if object, ok := value.(map[string]any); ok {
			// An object merge-patches its children, rather than replacing the whole map. `{}` is a no-op.
			if len(object) > 0 {
				if err := validatePatchObject(object, next, protected); err != nil {
					return err
				}
			}
			continue
		}

		pointer := pointerOf(next)
		for protectedPath, reason := range protected {
			if pointersOverlap(pointer, protectedPath) {
				return &PatchViolation{Path: protectedPath, Reason: reason}
			}
		}
	}
	return nil
}

func pointerOf(path []string) string {
	if len(path) == 0 {
		return ""
	}
	result := ""
	for _, segment := range path {
		result += "/" + escapePointer(segment)
	}
	return result
}

func pointersOverlap(a, b string) bool {
	return a == b || hasPointerPrefix(a, b) || hasPointerPrefix(b, a)
}

func hasPointerPrefix(path, prefix string) bool {
	return len(path) > len(prefix) && len(prefix) > 0 && path[:len(prefix)] == prefix && path[len(prefix)] == '/'
}
