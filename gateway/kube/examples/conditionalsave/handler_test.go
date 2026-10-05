package conditionalsave

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestHostPreservesCapturedVersionAndKubernetesConflict(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: "app", UID: "u", ResourceVersion: "2"}})
	var sent []byte
	cs.PrependReactor("patch", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		sent = action.(k8stesting.PatchAction).GetPatch()
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "cm", nil)
	})
	handler := Handler(func(*http.Request) (kubernetes.Interface, error) { return cs, nil }, "app", "cm")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(`{"uid":"u","resourceVersion":"1","patch":{"data":{"value":"mine"}}}`)))
	if response.Code != http.StatusConflict {
		t.Fatalf("got HTTP %d", response.Code)
	}
	if !bytes.Contains(sent, []byte(`"resourceVersion":"1"`)) || !bytes.Contains(sent, []byte(`"uid":"u"`)) {
		t.Fatalf("captured preconditions lost: %s", sent)
	}
	// The same object is still there: an ordinary stale write, reported as Kubernetes said it.
	status := decodeStatus(t, response)
	if status.Reason != metav1.StatusReasonConflict || status.Details == nil || status.Details.UID != "" {
		t.Fatalf("a stale write must stay Kubernetes' own conflict: %+v", status)
	}
}

var configmaps = corev1.SchemeGroupVersion.WithResource("configmaps")

func decodeStatus(t *testing.T, response *httptest.ResponseRecorder) metav1.Status {
	t.Helper()
	if ct := response.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want a JSON Status", ct)
	}
	var status metav1.Status
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil || status.Kind != "Status" || int(status.Code) != response.Code {
		t.Fatalf("not a Kubernetes Status (%v): %s", err, response.Body.String())
	}
	return status
}

// patchFails serves a PATCH of cm with err, after letting change rewrite what is stored. It counts
// requests so a test can tell a classification GET from a retried write.
func patchFails(t *testing.T, err error, change func(cs *fake.Clientset)) (*httptest.ResponseRecorder, *fake.Clientset) {
	t.Helper()
	cs := fake.NewSimpleClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: "app", UID: "u", ResourceVersion: "2"}})
	cs.PrependReactor("patch", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		change(cs)
		return true, nil, err
	})
	response := httptest.NewRecorder()
	Handler(func(*http.Request) (kubernetes.Interface, error) { return cs, nil }, "app", "cm").
		ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(`{"uid":"u","resourceVersion":"1","patch":{"data":{"value":"mine"}}}`)))
	return response, cs
}

func replace(cs *fake.Clientset) {
	_ = cs.Tracker().Delete(configmaps, "app", "cm")
	_ = cs.Tracker().Add(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: "app", UID: "u2", ResourceVersion: "3"}})
}

func verbs(cs *fake.Clientset) []string {
	var out []string
	for _, action := range cs.Actions() {
		out = append(out, action.GetVerb())
	}
	return out
}

// Kubernetes checks the captured resourceVersion first, so a deletion and recreation between the
// preflight GET and the PATCH arrives as a plain conflict. One GET tells the two apart.
func TestHostClassifiesAReplacementBehindAConflict(t *testing.T) {
	conflict := apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "cm", errors.New("the object has been modified"))
	uid := apierrors.NewInvalid(schema.GroupKind{Kind: "ConfigMap"}, "cm", field.ErrorList{field.Invalid(field.NewPath("metadata", "uid"), "u", "field is immutable")})
	for name, err := range map[string]error{"conflict": conflict, "uid rejected": uid} {
		t.Run(name, func(t *testing.T) {
			response, cs := patchFails(t, err, replace)
			status := decodeStatus(t, response)
			if response.Code != http.StatusConflict || status.Message != "object was replaced" || status.Details == nil || status.Details.UID != "u2" {
				t.Fatalf("HTTP %d %+v; want the replacement named", response.Code, status)
			}
			if got := strings.Join(verbs(cs), ","); got != "get,patch,get" {
				t.Fatalf("requests = %s; want one classifying GET and no retried PATCH", got)
			}
		})
	}
	response, _ := patchFails(t, conflict, func(cs *fake.Clientset) { _ = cs.Tracker().Delete(configmaps, "app", "cm") })
	if status := decodeStatus(t, response); response.Code != http.StatusNotFound || status.Reason != metav1.StatusReasonNotFound {
		t.Fatalf("deleted after the conflict: HTTP %d %+v", response.Code, status)
	}
}

// A validation error is the person's to fix, not a conflict to reload: it keeps its code, reason and
// field causes, and nothing is fetched to second-guess it.
func TestHostForwardsValidationErrorsUnchanged(t *testing.T) {
	invalid := apierrors.NewInvalid(schema.GroupKind{Kind: "ConfigMap"}, "cm", field.ErrorList{field.Invalid(field.NewPath("metadata", "labels"), "bad value", "a valid label must be 63 characters or less")})
	response, cs := patchFails(t, invalid, replace)
	status := decodeStatus(t, response)
	if response.Code != http.StatusUnprocessableEntity || status.Reason != metav1.StatusReasonInvalid {
		t.Fatalf("HTTP %d %+v; a validation error is not a conflict", response.Code, status)
	}
	if status.Details == nil || len(status.Details.Causes) != 1 || status.Details.Causes[0].Field != "metadata.labels" {
		t.Fatalf("field causes lost: %+v", status.Details)
	}
	if got := strings.Join(verbs(cs), ","); got != "get,patch" {
		t.Fatalf("requests = %s", got)
	}
}

func TestHostRejectsIdentityAndProjectionBypasses(t *testing.T) {
	for _, body := range []string{
		`{"uid":"u","resourceVersion":"1","patch":{"metadata":null}}`,
		`{"uid":"u","resourceVersion":"1","patch":{"metadata":{"resourceVersion":"2"}}}`,
		`{"uid":"u","resourceVersion":"1","patch":{"metadata":{"annotations":{"kubectl.kubernetes.io/last-applied-configuration":"secret"}}}}`,
		`{"uid":"u","resourceVersion":"1","patch":{"status":{}}}`,
		`{"uid":"u","patch":{"data":{"value":"mine"}}}`,
		`{"uid":"u","resourceVersion":"1","patch":{}} {}`,
	} {
		t.Run(body, func(t *testing.T) {
			cs := fake.NewSimpleClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: "app", UID: "u", ResourceVersion: "1"}})
			handler := Handler(func(*http.Request) (kubernetes.Interface, error) { return cs, nil }, "app", "cm")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(body)))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("got HTTP %d", response.Code)
			}
			for _, action := range cs.Actions() {
				if action.GetVerb() == "patch" {
					t.Fatal("rejected body reached Kubernetes PATCH")
				}
			}
		})
	}
}

func TestHostGETReturnsProjectedEnvelope(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "cm", Namespace: "app", UID: "u", ResourceVersion: "2",
		Annotations:   map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "private"},
		ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "host"}},
	}, Data: map[string]string{"value": "base"}})
	response := httptest.NewRecorder()
	Handler(func(*http.Request) (kubernetes.Interface, error) { return cs, nil }, "app", "cm").ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("got HTTP %d", response.Code)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope) != 2 || envelope["object"] == nil || envelope["redactedPaths"] == nil {
		t.Fatalf("wrong envelope: %s", response.Body.String())
	}
	var object corev1.ConfigMap
	if err := json.Unmarshal(envelope["object"], &object); err != nil {
		t.Fatal(err)
	}
	if object.UID != "u" || object.ResourceVersion != "2" || object.Data["value"] != "base" {
		t.Fatalf("lost object fields: %+v", object)
	}
	if len(object.ManagedFields) != 0 || object.Annotations["kubectl.kubernetes.io/last-applied-configuration"] != "" {
		t.Fatal("GET leaked stripped metadata")
	}
	var paths []string
	if err := json.Unmarshal(envelope["redactedPaths"], &paths); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Fatalf("unexpected ConfigMap redactions: %v", paths)
	}
}
