//go:build e2e

package sharedstream

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ConfigButler/krm-stream/gateway"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
)

// TestSharedHostRealAPI is manual evidence, included by task test-cluster. It needs
// EXCLUSIVE use of the cluster: activeConfigMapWatches reads a cluster-wide gauge with
// no namespace label, so any other client watching ConfigMaps is counted too. Run it
// with `go test -p 1`, never concurrently with the backend e2e suite. Set
// KRM_SHARED_SUBSCRIBERS=200 for the opening/reconnect burst profile. No login
// system or browser credentials are synthesized; the fixture supplies trusted sessions.
func TestSharedHostRealAPI(t *testing.T) {
	count := 2
	if value := os.Getenv("KRM_SHARED_SUBSCRIBERS"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 2 || n > 200 {
			t.Fatal("KRM_SHARED_SUBSCRIBERS must be 2..200")
		}
		count = n
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), nil).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.QPS = 100
	cfg.Burst = 400 // Declared fixture capacity, not a library default.
	admin, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ns, err := admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "krm-shared-"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{}) })
	_, err = admin.CoreV1().ConfigMaps(ns.Name).Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "coffee"}, Data: map[string]string{"value": "initial"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	makeToken := func(name string) string {
		t.Helper()
		_, err := admin.CoreV1().ServiceAccounts(ns.Name).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		token, err := admin.CoreV1().ServiceAccounts(ns.Name).CreateToken(ctx, name, &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: ptr.To(int64(3600))}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return token.Status.Token
	}
	serviceToken := makeToken("stream-host")
	for _, name := range []string{"shared-reader", "participant"} {
		verbs := []string{"list", "watch"}
		if name == "participant" {
			verbs = append(verbs, "get", "patch")
		}
		_, err := admin.RbacV1().Roles(ns.Name).Create(ctx, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{"coffee"}, Verbs: verbs}}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
	bind := func(name, role string) {
		t.Helper()
		_, err := admin.RbacV1().RoleBindings(ns.Name).Create(ctx, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}, RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: role}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: name, Namespace: ns.Name}}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
	bind("stream-host", "shared-reader")
	_, err = admin.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: ns.Name}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{"authorization.k8s.io"}, Resources: []string{"subjectaccessreviews"}, Verbs: []string{"create"}}}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = admin.RbacV1().ClusterRoles().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
	})
	_, err = admin.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: ns.Name}, RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: ns.Name}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: ns.Name, Name: "stream-host"}}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = admin.RbacV1().ClusterRoleBindings().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
	})
	tokens := make([]string, count)
	for i := range tokens {
		name := fmt.Sprintf("viewer-%d", i)
		tokens[i] = makeToken(name)
		bind(name, "participant")
	}
	serviceCfg := rest.AnonymousClientConfig(cfg)
	serviceCfg.BearerToken = serviceToken
	counters := &Counters{}
	h, err := Handler(serviceCfg, ns.Name, "coffee", func(r *http.Request) (Session, error) {
		// Fixture-only routing to trusted sessions. Production resolves an authenticated cookie.
		i, err := strconv.Atoi(r.URL.Query().Get("fixture_session"))
		if err != nil || i < 0 || i >= count {
			return Session{}, fmt.Errorf("unknown fixture session")
		}
		return Session{Token: tokens[i], SessionExpiry: time.Now().Add(2 * time.Minute), TokenExpiry: time.Now().Add(time.Hour)}, nil
	}, counters)
	if err != nil {
		t.Fatal(err)
	}
	baseline := activeConfigMapWatches(ctx, t, admin)
	host := httptest.NewServer(h)
	defer host.Close()
	type stream struct {
		body   *http.Response
		events chan gateway.Event
		done   chan struct{}
	}
	streams := make([]stream, count)
	open := func(i int) error {
		request, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/?version=v1&resource=configmaps&namespace=%s&name=coffee&fixture_session=%d", host.URL, ns.Name, i), nil)
		if err != nil {
			return err
		}
		res, err := host.Client().Do(request)
		if err != nil {
			return err
		}
		st := stream{body: res, events: make(chan gateway.Event, 32), done: make(chan struct{})}
		streams[i] = st
		go func() {
			defer close(st.done)
			defer close(st.events)
			scanner := bufio.NewScanner(res.Body)
			for scanner.Scan() {
				if line := scanner.Text(); strings.HasPrefix(line, "data: ") {
					var ev gateway.Event
					if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
						return
					}
					select {
					case st.events <- ev:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
		return nil
	}
	defer func() {
		for _, s := range streams {
			if s.body != nil {
				_ = s.body.Body.Close()
			}
		}
	}()
	wait := func(i int, kind gateway.EventType) {
		t.Helper()
		for {
			select {
			case ev, ok := <-streams[i].events:
				if !ok {
					t.Fatalf("stream %d ended before %s", i, kind)
				}
				if ev.Type == kind {
					return
				}
				if ev.Type == gateway.EventError {
					t.Fatalf("stream %d: %+v", i, ev)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
	}
	started := time.Now()
	var wg sync.WaitGroup
	for i := range count {
		wg.Go(func() {
			if err := open(i); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	for i := range count {
		wait(i, gateway.EventSynced)
	}
	t.Logf("%d independent identities synced in %s", count, time.Since(started))
	active := activeConfigMapWatches(ctx, t, admin)
	if active != baseline+1 {
		t.Fatalf("API watch count baseline=%g active=%g", baseline, active)
	}
	t.Logf("API-server active ConfigMap WATCH requests: baseline=%g active=%g", baseline, active)
	// Reconnect a quarter of the cohort while others retain the shared watch.
	for i := range max(1, count/4) {
		_ = streams[i].body.Body.Close()
		<-streams[i].done
		if err := open(i); err != nil {
			t.Fatal(err)
		}
		wait(i, gateway.EventSynced)
	}
	if got := activeConfigMapWatches(ctx, t, admin); got != active {
		t.Fatalf("reconnect changed watch count: %g", got)
	}
	participantCfg := rest.AnonymousClientConfig(cfg)
	participantCfg.BearerToken = tokens[1]
	participant, err := kubernetes.NewForConfig(participantCfg)
	if err != nil {
		t.Fatal(err)
	}
	patch := func(value string) {
		t.Helper()
		_, err := participant.CoreV1().ConfigMaps(ns.Name).Patch(ctx, "coffee", types.MergePatchType, []byte(fmt.Sprintf(`{"data":{"value":%q}}`, value)), metav1.PatchOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
	patch("updated")
	for i := range count {
		wait(i, gateway.EventModified)
	}
	started = time.Now()
	if err := admin.RbacV1().RoleBindings(ns.Name).Delete(ctx, "viewer-0", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	wait(0, gateway.EventError)
	select {
	case <-streams[0].done:
	case <-time.After(60 * time.Second):
		t.Fatal("revoked stream did not close")
	}
	if time.Since(started) > 60*time.Second {
		t.Fatal("revocation exceeded reference budget")
	}
	t.Logf("revoked identity closed in %s; %d peers retained", time.Since(started), count-1)
	patch("after-revocation")
	for i := 1; i < count; i++ {
		wait(i, gateway.EventModified)
	}
	for i := range streams {
		_ = streams[i].body.Body.Close()
		<-streams[i].done
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := activeConfigMapWatches(ctx, t, admin)
		if got == baseline && counters.Streams.Load() == 0 && counters.Subscriptions.Load() == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cleanup: API=%g baseline=%g streams=%d subscriptions=%d", got, baseline, counters.Streams.Load(), counters.Subscriptions.Load())
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Log("final API watch count and host gauges returned to baseline")
}

func activeConfigMapWatches(ctx context.Context, t *testing.T, admin kubernetes.Interface) float64 {
	t.Helper()
	raw, err := admin.CoreV1().RESTClient().Get().AbsPath("/metrics").DoRaw(ctx)
	if err != nil {
		t.Fatal(err)
	}
	total := float64(0)
	found := false
	for line := range strings.SplitSeq(string(raw), "\n") {
		if strings.HasPrefix(line, "apiserver_longrunning_requests{") && strings.Contains(line, `verb="WATCH"`) && strings.Contains(line, `resource="configmaps"`) && strings.Contains(line, `group=""`) {
			found = true
			fields := strings.Fields(line)
			value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
			if err != nil {
				t.Fatal(err)
			}
			total += value
		}
	}
	if !found {
		t.Fatal("API-server ConfigMap WATCH metric missing; cannot establish physical watch counts")
	}
	return total
}
