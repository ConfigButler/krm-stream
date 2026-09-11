// Package conditionalsave is a host-owned ConfigMap editor endpoint example. Copy and adapt it;
// this is intentionally outside the library API. Mount it on a fixed, host-authorized namespace/name.
package conditionalsave

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/ConfigButler/krm-stream/gateway"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

type saveRequest struct {
	UID             string         `json:"uid"`
	ResourceVersion string         `json:"resourceVersion"`
	Patch           map[string]any `json:"patch"`
}

// Handler uses a host-authenticated, caller-scoped Kubernetes client on every request. The host
// owns session validation, CSRF protection, routing and audit. This example pins ProjectionFull,
// which must also be the stream's effective projection. clientFor must enforce access to this route.
func Handler(clientFor func(*http.Request) (kubernetes.Interface, error), namespace, name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet && r.Method != http.MethodPatch {
			w.Header().Set("Allow", "GET, PATCH")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		cs, err := clientFor(r)
		if err != nil {
			http.Error(w, "not authenticated", http.StatusUnauthorized)
			return
		}
		client := cs.CoreV1().ConfigMaps(namespace)
		current, err := client.Get(r.Context(), name, metav1.GetOptions{})
		if err != nil {
			writeError(w, err)
			return
		}
		object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(current)
		if err != nil {
			http.Error(w, "conversion failed", http.StatusInternalServerError)
			return
		}
		if r.Method == http.MethodGet {
			projected, _ := gateway.Project(gateway.ProjectionFull, object)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(struct {
				Object   gateway.KRMObject   `json:"object"`
				Redacted []gateway.Redaction `json:"redacted"`
			}{projected, []gateway.Redaction{}})
			return
		}
		var request saveRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&request); err != nil {
			http.Error(w, "invalid save request", http.StatusBadRequest)
			return
		}
		if err = decoder.Decode(new(any)); err != io.EOF {
			http.Error(w, "expected one save request", http.StatusBadRequest)
			return
		}
		if request.UID == "" || request.ResourceVersion == "" || request.Patch == nil {
			http.Error(w, "UID, resourceVersion and patch are required", http.StatusBadRequest)
			return
		}
		if request.UID != string(current.UID) {
			http.Error(w, "object was replaced", http.StatusConflict)
			return
		}
		// Application edit policy is separate from projection protection. Metadata identity and version
		// are never accepted inside the user's patch; the host sets the captured preconditions below.
		for key, value := range request.Patch {
			switch key {
			case "data", "binaryData":
			case "metadata":
				metadata, ok := value.(map[string]any)
				if !ok {
					http.Error(w, "metadata must be an object", http.StatusBadRequest)
					return
				}
				for field := range metadata {
					if field != "labels" && field != "annotations" {
						http.Error(w, "metadata field is not editable", http.StatusBadRequest)
						return
					}
				}
			default:
				http.Error(w, "field is not editable", http.StatusBadRequest)
				return
			}
		}
		metadata, _ := request.Patch["metadata"].(map[string]any)
		if metadata == nil {
			metadata = map[string]any{}
			request.Patch["metadata"] = metadata
		}
		metadata["uid"] = request.UID
		metadata["resourceVersion"] = request.ResourceVersion // NEVER current.ResourceVersion
		patch, err := json.Marshal(request.Patch)
		if err != nil {
			http.Error(w, "invalid patch", http.StatusBadRequest)
			return
		}
		if err = gateway.ValidateMergePatch(gateway.ProjectionFull, object, patch); err != nil {
			http.Error(w, "patch touches a protected field", http.StatusBadRequest)
			return
		}
		_, err = client.Patch(r.Context(), name, types.MergePatchType, patch, metav1.PatchOptions{})
		if err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent) // The projected watch echo settles dirty state.
	})
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	var apiStatus apierrors.APIStatus
	if errors.As(err, &apiStatus) {
		status = int(apiStatus.Status().Code)
	}
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	http.Error(w, http.StatusText(status), status)
}
