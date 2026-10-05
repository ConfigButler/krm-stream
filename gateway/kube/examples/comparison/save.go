package comparison

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/ConfigButler/krm-stream/gateway"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// The host's save endpoints for gateway editing, one per projection, built the way
// examples/conditionalsave is: a projected GET envelope for guarded recovery reads, and a PATCH that
// carries the captured UID and resourceVersion as Kubernetes preconditions, after
// gateway.ValidateMergePatch has checked it against the same projection the stream uses.
//
//	GET   /save/{full|spec}/widgets/{name}  → {"object": <projected>, "redactedPaths": [...]}
//	PATCH /save/{full|spec}/widgets/{name}  ← {"uid", "resourceVersion", "patch"}  → 204
//
// Only a Widget's spec is editable. Secrets are not editable through the gateway: their values are
// redacted, and a projected editor has nothing it could write.

type saveRequest struct {
	UID             string         `json:"uid"`
	ResourceVersion string         `json:"resourceVersion"`
	Patch           map[string]any `json:"patch"`
}

func (h *Host) saveHandler(w http.ResponseWriter, r *http.Request) {
	m := h.metrics
	w.Header().Set("Cache-Control", "no-store")
	rest, _ := strings.CutPrefix(r.URL.Path, "/save/")
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[1] != "widgets" || parts[2] == "" {
		http.NotFound(w, r)
		return
	}
	var projection gateway.Projection
	switch parts[0] {
	case "full":
		projection = gateway.ProjectionFull
	case "spec":
		projection = gateway.ProjectionSpec
	default:
		http.NotFound(w, r)
		return
	}
	name := parts[2]
	session, ok := h.authenticate(w, r, "save")
	if !ok {
		return
	}
	if session.refused {
		writeStatus(w, metav1.Status{Code: http.StatusForbidden, Reason: metav1.StatusReasonForbidden, Message: "this session may not edit these objects"})
		return
	}
	prefix := "save." + parts[0]
	writer := &countingWriter{ResponseWriter: w, add: func(n int64) { m.Add(prefix+".bytes", n) }}
	method := strings.ToLower(r.Method)
	if r.Method != http.MethodGet && r.Method != http.MethodPatch {
		w.Header().Set("Allow", "GET, PATCH")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	m.Add(prefix+"."+method+"_requests", 1)
	defer func() { m.Add(prefix+"."+method+"_status."+statusClass(writer.status), 1) }()

	client := h.objects.widgetClient()
	current, err := client.Get(r.Context(), name, metav1.GetOptions{})
	if err != nil {
		writeError(writer, err)
		return
	}
	if r.Method == http.MethodGet {
		projected, paths := gateway.Project(projection, current.Object)
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(struct {
			Object        gateway.KRMObject `json:"object"`
			RedactedPaths []string          `json:"redactedPaths"`
		}{projected, paths})
		return
	}
	var request saveRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeStatus(writer, metav1.Status{Code: http.StatusBadRequest, Message: "invalid save request"})
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeStatus(writer, metav1.Status{Code: http.StatusBadRequest, Message: "expected one save request"})
		return
	}
	if request.UID == "" || request.ResourceVersion == "" || request.Patch == nil {
		writeStatus(writer, metav1.Status{Code: http.StatusBadRequest, Message: "UID, resourceVersion and patch are required"})
		return
	}
	if request.UID != string(current.GetUID()) {
		writeReplaced(writer, name, current.GetUID())
		return
	}
	// Application edit policy, separate from projection protection: a Widget's spec, nothing else.
	// Identity and version are never accepted inside the user's patch; the host sets the captured
	// preconditions below.
	for key := range request.Patch {
		if key != "spec" {
			writeStatus(writer, metav1.Status{Code: http.StatusBadRequest, Message: "only spec is editable here"})
			return
		}
	}
	request.Patch["metadata"] = map[string]any{"uid": request.UID, "resourceVersion": request.ResourceVersion}
	patch, err := json.Marshal(request.Patch)
	if err != nil {
		writeStatus(writer, metav1.Status{Code: http.StatusBadRequest, Message: "invalid patch"})
		return
	}
	if err := gateway.ValidateMergePatch(projection, current.Object, patch); err != nil {
		writeStatus(writer, metav1.Status{Code: http.StatusBadRequest, Message: "patch touches a protected field"})
		return
	}
	_, err = client.Patch(r.Context(), name, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil && (apierrors.IsConflict(err) || uidRejected(err)) {
		// Kubernetes checks the captured resourceVersion before the UID, so an object deleted and
		// recreated since the preflight GET fails exactly like a stale write. Ask which it was.
		latest, getErr := client.Get(r.Context(), name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(getErr):
			err = getErr
		case getErr == nil && latest.GetUID() != current.GetUID():
			writeReplaced(writer, name, latest.GetUID())
			return
		}
	}
	if err != nil {
		writeError(writer, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent) // The projected watch echo settles dirty state.
}

// writeError answers with the Kubernetes Status behind err. An error that is not an API response is
// reported without its text, which can name internal addresses.
func writeError(w http.ResponseWriter, err error) {
	var apiStatus apierrors.APIStatus
	if errors.As(err, &apiStatus) {
		writeStatus(w, apiStatus.Status())
		return
	}
	writeStatus(w, metav1.Status{Code: http.StatusBadGateway, Message: "the Kubernetes request failed"})
}

func writeReplaced(w http.ResponseWriter, name string, current types.UID) {
	writeStatus(w, metav1.Status{
		Code:    http.StatusConflict,
		Reason:  metav1.StatusReasonConflict,
		Message: "object was replaced",
		Details: &metav1.StatusDetails{Name: name, Kind: "widgets", UID: current},
	})
}

// refuse answers as the API server would, with a Status an editor can show.
func refuse(w http.ResponseWriter, code int, message string) {
	writeStatus(w, metav1.Status{Code: int32(code), Message: message}) //nolint:gosec // an HTTP status code
}

func writeStatus(w http.ResponseWriter, status metav1.Status) {
	if status.Code < 400 || status.Code > 599 {
		status.Code = http.StatusBadGateway
	}
	status.TypeMeta = metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}
	status.Status = metav1.StatusFailure
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(int(status.Code))
	_ = json.NewEncoder(w).Encode(status)
}

// uidRejected reports a 422 whose cause is metadata.uid: the identity precondition.
func uidRejected(err error) bool {
	var apiStatus apierrors.APIStatus
	if !apierrors.IsInvalid(err) || !errors.As(err, &apiStatus) || apiStatus.Status().Details == nil {
		return false
	}
	for _, cause := range apiStatus.Status().Details.Causes {
		if cause.Field == "metadata.uid" {
			return true
		}
	}
	return false
}
