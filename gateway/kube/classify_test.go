package kube

import (
	"errors"
	"net"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"

	"github.com/ConfigButler/krm-stream/gateway"
)

// Every way an API server can refuse, on every path an error can take, maps to the spec's code
// (§4.3). Before this, every one of them reached a browser as a terminal INTERNAL carrying the
// cluster's internal URL.
func TestAPIServerErrorsMapToProtocolCodes(t *testing.T) {
	gr := schema.GroupResource{Group: "hello.krm-foyer.example", Resource: "notes"}
	for _, tc := range []struct {
		name       string
		err        error
		code       gateway.ErrorCode
		terminal   bool
		retryAfter int
		message    string
	}{
		{"403", apierrors.NewForbidden(gr, "", errors.New(`User "oidc:carol@example.com" cannot watch resource "notes"`)),
			gateway.CodeForbidden, true, 0, `cannot watch resource "notes"`},
		{"401", apierrors.NewUnauthorized("token expired"), gateway.CodeUnauthenticated, true, 0, ""},
		{"404", apierrors.NewNotFound(gr, ""), gateway.CodeScopeInvalid, true, 0, ""},
		{"429", apierrors.NewTooManyRequests("slow down", 7), gateway.CodeUpstreamUnavailable, false, 7000, ""},
		{"500", apierrors.NewInternalError(errors.New("etcd")), gateway.CodeUpstreamUnavailable, false, 0, ""},
		{"503", apierrors.NewServiceUnavailable("restarting"), gateway.CodeUpstreamUnavailable, false, 0, ""},
		{"504", apierrors.NewTimeoutError("slow", 0), gateway.CodeUpstreamUnavailable, false, 0, ""},
		{"connection refused", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")},
			gateway.CodeUpstreamUnavailable, false, 0, ""},
	} {
		for _, path := range []string{"streaming list", "fallback list", "fallback watch"} {
			t.Run(tc.name+"/"+path, func(t *testing.T) {
				client, res := newStub(t)
				res.watchFn = func(o metav1.ListOptions) (watch.Interface, error) {
					if path == "streaming list" || (path == "fallback watch" && o.SendInitialEvents == nil) {
						return nil, tc.err
					}
					return nil, refusal()
				}
				res.listFn = func(metav1.ListOptions) (*unstructured.UnstructuredList, error) {
					if path == "fallback list" {
						return nil, tc.err
					}
					return &unstructured.UnstructuredList{}, nil
				}
				_, err := NewBackend(client).Watch(t.Context(), scope)
				_ = assertCode(t, err, tc.code, tc.terminal, tc.retryAfter, tc.message)
			})
		}
	}
}

// The same rules hold for an ERROR event on an established watch: a grant revoked mid-stream is as
// terminal as one refused at opening. Only 410, and what cannot be classified, start a new cycle.
func TestWatchErrorEventsAreClassified(t *testing.T) {
	for _, tc := range []struct {
		status   metav1.Status
		code     gateway.ErrorCode
		terminal bool
	}{
		{metav1.Status{Code: 403, Reason: metav1.StatusReasonForbidden, Message: "revoked"}, gateway.CodeForbidden, true},
		{metav1.Status{Code: 401, Reason: metav1.StatusReasonUnauthorized}, gateway.CodeUnauthenticated, true},
		{metav1.Status{Code: 500, Reason: metav1.StatusReasonInternalError}, gateway.CodeUpstreamUnavailable, false},
		{metav1.Status{Code: 410, Reason: metav1.StatusReasonExpired}, gateway.CodeResyncRequired, false},
		{metav1.Status{Code: 422, Reason: metav1.StatusReasonInvalid}, gateway.CodeResyncRequired, false},
	} {
		t.Run(string(tc.status.Reason), func(t *testing.T) {
			client, res := newStub(t)
			fake := watch.NewFakeWithChanSize(1, false)
			res.watchFn = func(metav1.ListOptions) (watch.Interface, error) { return fake, nil }
			w, err := NewBackend(client).Watch(t.Context(), scope)
			if err != nil {
				t.Fatalf("Watch: %v", err)
			}
			defer w.Stop()
			status := tc.status
			status.Status = metav1.StatusFailure
			fake.Error(&status)
			_ = assertCode(t, drain(t, w, 1)[0].Err, tc.code, tc.terminal, 0, "")
		})
	}
}

// A real dial against a port nobody listens on: the browser hears UPSTREAM_UNAVAILABLE, and nothing
// about where the gateway was dialing. The address survives only as the Cause.
func TestAnUnreachableAPIServerIsUpstreamUnavailable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	b, err := NewBackendForConfig(&rest.Config{Host: "http://" + addr})
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Watch(t.Context(), scope)
	se := assertCode(t, err, gateway.CodeUpstreamUnavailable, false, 0, "")
	if strings.Contains(se.Message, addr) || !strings.Contains(se.Cause.Error(), addr) {
		t.Errorf("message %q must not name %s; the cause %q must", se.Message, addr, se.Cause)
	}
}

func assertCode(t *testing.T, err error, code gateway.ErrorCode, terminal bool, retryAfter int, message string) *gateway.StreamError {
	t.Helper()
	var se *gateway.StreamError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v (%T), want a *gateway.StreamError", err, err)
	}
	if se.Code != code || se.Terminal != terminal {
		t.Errorf("got %s terminal=%v, want %s terminal=%v (%v)", se.Code, se.Terminal, code, terminal, err)
	}
	got := 0
	if se.RetryAfterMs != nil {
		got = *se.RetryAfterMs
	}
	if got != retryAfter {
		t.Errorf("retryAfterMs = %d, want %d", got, retryAfter)
	}
	if !strings.Contains(se.Message, message) {
		t.Errorf("message = %q, want it to contain %q", se.Message, message)
	}
	if se.Cause == nil {
		t.Error("the raw error was dropped; Diagnostics needs it")
	}
	return se
}
