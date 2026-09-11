// Package sharedstream is a copyable host example, not a library authentication API.
package sharedstream

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/ConfigButler/krm-stream/gateway"
	"github.com/ConfigButler/krm-stream/gateway/kube"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Session comes from the host's trusted session store, never from browser identity
// headers. The host must reject invalid/revoked sessions and supply both expiries.
type Session struct {
	Token         string
	SessionExpiry time.Time
	TokenExpiry   time.Time
}

type sessionKey struct{}
type resolvedSession struct {
	session Session
	err     error
}

// Handler constructs one shared backend for one fixed ConfigMap on one cluster.
// Call once at process startup. cluster supplies service credentials and server/TLS
// settings. Participant clients use only those server/TLS settings plus the trusted
// session token. The session callback must honor r.Context and own authentication.
// Direct GET/PATCH routes must independently use participant credentials.
func Handler(cluster *rest.Config, namespace, name string, sessionFor func(*http.Request) (Session, error), observer gateway.Observer) (http.Handler, error) {
	if cluster == nil || namespace == "" || name == "" || sessionFor == nil {
		return nil, fmt.Errorf("sharedstream: cluster, fixed scope and session resolver required")
	}
	service, err := kubernetes.NewForConfig(cluster)
	if err != nil {
		return nil, err
	}
	data, err := dynamic.NewForConfig(cluster)
	if err != nil {
		return nil, err
	}
	shared := gateway.NewSharedBackendWithOptions(kube.NewBackend(data), gateway.SharedOptions{Observer: observer})
	sar := kube.SubjectAccessReviewAuthorizer(service, func(p gateway.Principal) (kube.Subject, error) {
		subject, ok := p.(kube.Subject)
		if !ok {
			return kube.Subject{}, fmt.Errorf("missing resolved subject")
		}
		return subject, nil
	})
	participantConfig := rest.AnonymousClientConfig(cluster)
	// AnonymousClientConfig strips credential-bearing transports and impersonation.
	// This client is used for one bounded SSR, not the long-lived shared watch.
	participantConfig.Timeout = 5 * time.Second
	handler := gateway.Handler(gateway.Options{
		Principal: func(r *http.Request) (gateway.Principal, error) {
			state := r.Context().Value(sessionKey{}).(resolvedSession)
			if state.err != nil {
				return nil, state.err
			}
			cfg := rest.CopyConfig(participantConfig)
			cfg.BearerToken = state.session.Token
			client, err := kubernetes.NewForConfig(cfg)
			if err != nil {
				return nil, err
			}
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			return resolveSubject(ctx, client)
		},
		Authorizer: gateway.AuthorizerFunc(func(ctx context.Context, p gateway.Principal, s gateway.Scope) error {
			if s.Target != "" || s.Group != "" || s.Version != "v1" || s.Resource != "configmaps" || s.Namespace != namespace || s.Name != name || s.LabelSelector != "" {
				return gateway.Forbidden("outside the fixed stream scope")
			}
			// Applies to initial/cycle checks too; the periodic setting alone does not.
			check, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			return sar.Authorize(check, p, s)
		}),
		Clients:      func(context.Context, string, gateway.Principal) (gateway.Backend, error) { return shared, nil },
		Scopes:       gateway.ScopePolicy{Targets: []string{""}, Resources: []gateway.GroupResource{{Resource: "configmaps", Scope: gateway.ResourceScopeNamespaced}}},
		Projection:   gateway.ProjectionFull,
		WriteTimeout: 5 * time.Second, ReauthorizationInterval: 30 * time.Second, ReauthorizationTimeout: 5 * time.Second,
		Observer: observer,
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, err := sessionFor(r)
		expiry := session.SessionExpiry
		if session.TokenExpiry.Before(expiry) {
			expiry = session.TokenExpiry
		}
		if err == nil && (session.Token == "" || session.SessionExpiry.IsZero() || session.TokenExpiry.IsZero() || !expiry.After(time.Now())) {
			err = fmt.Errorf("invalid or expired session")
		}
		ctx := r.Context()
		if err == nil {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, expiry)
			defer cancel()
		}
		ctx = context.WithValue(ctx, sessionKey{}, resolvedSession{session: session, err: err})
		handler.ServeHTTP(w, r.WithContext(ctx))
	}), nil
}
