package conditionalsave

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
