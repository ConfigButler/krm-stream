package sharedstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ConfigButler/krm-stream/gateway"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
)

func TestResolveSubjectOwnershipAndErrors(t *testing.T) {
	info := authenticationv1.UserInfo{Username: "mapped:alice", UID: "uid", Groups: []string{"a", "b"}, Extra: map[string]authenticationv1.ExtraValue{"key": {"one", "two"}}}
	for _, scenario := range []string{"success", "empty", "nil", "failure"} {
		t.Run(scenario, func(t *testing.T) {
			result := &authenticationv1.SelfSubjectReview{Status: authenticationv1.SelfSubjectReviewStatus{UserInfo: info}}
			apiErr := errors.New("API unavailable")
			client := fake.NewClientset()
			client.PrependReactor("create", "selfsubjectreviews", func(clienttesting.Action) (bool, runtime.Object, error) {
				switch scenario {
				case "empty":
					return true, &authenticationv1.SelfSubjectReview{}, nil
				case "nil":
					return true, nil, nil
				case "failure":
					return true, nil, apiErr
				default:
					return true, result, nil
				}
			})
			subject, err := resolveSubject(t.Context(), client)
			if scenario != "success" {
				if err == nil {
					t.Fatal("expected error")
				}
				if scenario == "failure" && !errors.Is(err, apiErr) {
					t.Fatal(err)
				}
				return
			}
			if subject.User != info.Username || subject.UID != info.UID || !reflect.DeepEqual(subject.Groups, info.Groups) || !reflect.DeepEqual([]string(subject.Extra["key"]), []string(info.Extra["key"])) {
				t.Fatal(subject)
			}
			subject.Groups[0] = "changed"
			subject.Extra["key"][0] = "changed"
			if result.Status.UserInfo.Groups[0] != "a" || result.Status.UserInfo.Extra["key"][0] != "one" {
				t.Fatal("subject aliases API response")
			}
		})
	}
}

func TestResolveSubjectInflightCancellation(t *testing.T) {
	entered := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
	}))
	defer srv.Close()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := resolveSubject(ctx, client); done <- err }()
	<-entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("SSR did not cancel")
	}
}

func TestHostBoundaryUsesResolvedSubjectAndFixedScope(t *testing.T) {
	var mu sync.Mutex
	var subjects []authorizationv1.SubjectAccessReviewSpec
	var tokens []string
	var watches int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "selfsubjectreviews"):
			mu.Lock()
			tokens = append(tokens, r.Header.Get("Authorization"))
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(&authenticationv1.SelfSubjectReview{Status: authenticationv1.SelfSubjectReviewStatus{UserInfo: authenticationv1.UserInfo{Username: "resolved:user", UID: "resolved:uid", Groups: []string{"resolved:group"}, Extra: map[string]authenticationv1.ExtraValue{"x": {"a", "b"}}}}})
		case strings.HasSuffix(r.URL.Path, "subjectaccessreviews"):
			var sar authorizationv1.SubjectAccessReview
			if err := json.NewDecoder(r.Body).Decode(&sar); err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			subjects = append(subjects, sar.Spec)
			tokens = append(tokens, r.Header.Get("Authorization"))
			mu.Unlock()
			sar.Status.Allowed = true
			_ = json.NewEncoder(w).Encode(&sar)
		case strings.HasSuffix(r.URL.Path, "configmaps"):
			mu.Lock()
			watches++
			tokens = append(tokens, r.Header.Get("Authorization"))
			mu.Unlock()
			if r.URL.Query().Get("fieldSelector") != "metadata.name=coffee" {
				t.Error("missing named scope")
			}
			_, _ = io.WriteString(w, "{\"type\":\"BOOKMARK\",\"object\":{\"apiVersion\":\"v1\",\"kind\":\"ConfigMap\",\"metadata\":{\"resourceVersion\":\"1\",\"annotations\":{\"k8s.io/initial-events-end\":\"true\"}}}}\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	counters := &Counters{}
	h, err := Handler(&rest.Config{Host: api.URL, BearerToken: "service", ContentConfig: rest.ContentConfig{ContentType: "application/json"}}, "app", "coffee", func(*http.Request) (Session, error) {
		return Session{Token: "participant", SessionExpiry: time.Now().Add(150 * time.Millisecond), TokenExpiry: time.Now().Add(time.Hour)}, nil
	}, counters)
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(h)
	defer host.Close()
	for _, name := range []string{"outside", "coffee"} {
		req, err := http.NewRequestWithContext(t.Context(), "GET", host.URL+"/?version=v1&resource=configmaps&namespace=app&name="+name, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Remote-User", "attacker")
		req.Header.Set("Authorization", "Bearer attacker")
		res, err := host.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if name == "outside" && !strings.Contains(string(body), "FORBIDDEN") {
			t.Fatalf("scope escaped: %s", body)
		}
		if name == "coffee" && !strings.Contains(string(body), "synced") {
			t.Fatalf("no snapshot: %s", body)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if watches != 1 || len(subjects) != 2 {
		t.Fatalf("watches=%d SARs=%d", watches, len(subjects))
	}
	for _, s := range subjects {
		if s.User != "resolved:user" || s.UID != "resolved:uid" || s.Groups[0] != "resolved:group" || len(s.Extra["x"]) != 2 || s.ResourceAttributes.Name != "coffee" {
			t.Fatal(s)
		}
	}
	if strings.Join(tokens, ",") != "Bearer participant,Bearer participant,Bearer service,Bearer service,Bearer service" {
		t.Fatal(tokens)
	}
	if counters.Streams.Load() != 0 || counters.Subscriptions.Load() != 0 {
		t.Fatal("unbalanced lifetimes")
	}
}

func TestCounterMappingConcurrentAndUnknown(t *testing.T) {
	c := &Counters{}
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			for _, kind := range []gateway.ObservationKind{gateway.ObservationStreamOpened, gateway.ObservationSharedSubscriptionOpened, "future_kind", gateway.ObservationSharedSubscriptionClosed, gateway.ObservationStreamClosed} {
				c.Observe(gateway.Observation{Kind: kind})
			}
		})
	}
	wg.Wait()
	c.Observe(gateway.Observation{Kind: gateway.ObservationHTTPTransportRejected})
	if c.Streams.Load() != 0 || c.Subscriptions.Load() != 0 || c.TransportRejected.Load() != 1 {
		t.Fatal("counter mapping is unbalanced")
	}
}
