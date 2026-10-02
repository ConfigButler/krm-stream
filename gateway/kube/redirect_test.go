package kube

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"k8s.io/client-go/rest"

	"github.com/ConfigButler/krm-stream/gateway"
)

// client-go follows redirects, and its transport puts the bearer token on the redirected request
// too. A backend holding the caller's token would hand it to wherever a redirect points: another
// host, or plain http. An aggregated API's answer passes back through the API server, so this is
// not only a hostile-server case. The backend refuses redirects, and the refusal is terminal: a
// redirect does not go away on retry.
func TestABackendNeverFollowsARedirectWithTheCallersToken(t *testing.T) {
	const token = "caller-token-that-must-not-travel"
	var reached atomic.Int32
	recorder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		if strings.Contains(r.Header.Get("Authorization"), token) {
			t.Errorf("the caller's token reached the redirect target: %s %s", r.Method, r.URL)
		}
		http.Error(w, "recorded", http.StatusTeapot)
	}))
	defer recorder.Close()
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, recorder.URL+"/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer apiServer.Close()

	b, err := NewBackendForConfig(&rest.Config{Host: apiServer.URL, BearerToken: token})
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Watch(t.Context(), scope)
	if n := reached.Load(); n != 0 {
		t.Fatalf("the redirect was followed %d times", n)
	}
	var se *gateway.StreamError
	if !errors.As(err, &se) || se.Code != gateway.CodeInternal || !se.Terminal {
		t.Fatalf("err = %v, want a terminal INTERNAL: a redirect does not go away on retry", err)
	}
	if strings.Contains(se.Message, recorder.URL) || strings.Contains(se.Message, apiServer.URL) {
		t.Errorf("message %q names an address", se.Message)
	}
	if !errors.Is(err, ErrRedirectRefused) {
		t.Errorf("err = %v, want it to wrap ErrRedirectRefused for the host's diagnostics", err)
	}
}
