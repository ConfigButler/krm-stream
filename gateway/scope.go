package gateway

import (
	"net/url"
	"strings"
)

// The REQUEST half of the wire.
//
// v1 specified the scope's FIELDS and never its ENCODING (spec §8), and the cost of that omission was
// immediate: the replay server invented `?group=&version=&resource=`, the client's README invented a
// URL string, and nothing compared them. Two ends of one repository, free to disagree, with a green
// suite. So the encoding is pinned here, and — more importantly — in conformance/scopes.yaml, which
// BOTH suites read: the client builds the canonical query, the gateway parses it back.
//
// Two rules do real work here, and neither is cosmetic:
//
//  1. **A raw API-server address is REFUSED, not ignored.** Spec §8 says the browser never supplies
//     one. It would be easy to satisfy that by simply not having a parameter for it — but then
//     `?server=https://10.0.0.1:6443` is silently dropped, and an SSRF probe looks exactly like a
//     successful request. Refusing loudly turns a security property into an observable one.
//
//  2. **Every scope field is validated as the DNS-ish name it actually is.** A namespace of
//     `../../kube-system` is not a namespace; it is someone testing whether we paste this into a
//     path somewhere. We do not — but the next person to touch the code might, and by then the
//     hostile value is already inside a validated-looking Scope.

// forbiddenParams are query parameters this gateway refuses OUTRIGHT rather than ignores. Each one is
// an attempt (or a mistake) that says "let me tell you which API server to talk to" — and the answer
// to that is never "sure, but I'll ignore it".
var forbiddenParams = []string{"server", "apiserver", "api-server", "url", "endpoint", "kubeconfig", "token"}

// ScopeFromQuery parses and validates a scope from a URL query, or explains why it will not.
//
// It returns `error`, not `*StreamError`, and that is deliberate — see the note on ScopePolicy.Validate.
// The concrete value IS a *StreamError with CodeScopeInvalid and Terminal set (a browser's EventSource
// reconnects on its own, so a non-terminal rejection would have it hammer a malformed scope forever);
// reach it with errors.As when you need the code.
//
// Unknown parameters are TOLERATED (a host may carry its own — the replay server has `fixture` and
// `pace`) except for the ones in forbiddenParams, which are the ones that would mean something
// dangerous if we were the kind of gateway that honoured them.
func ScopeFromQuery(q url.Values) (Scope, error) {
	for _, p := range forbiddenParams {
		for key := range q {
			if strings.EqualFold(key, p) {
				return Scope{}, ScopeInvalid("this gateway never accepts an API-server address, endpoint or credential " +
					"from a caller: the host resolves the target (parameter: " + key + ")")
			}
		}
	}

	s := Scope{
		Target:        q.Get("target"),
		Group:         q.Get("group"),
		Version:       q.Get("version"),
		Resource:      q.Get("resource"),
		Namespace:     q.Get("namespace"),
		Name:          q.Get("name"),
		LabelSelector: q.Get("labelSelector"),
	}

	// No defaults, on purpose. A defaulted `version` streams objects of a shape the caller did not
	// ask for (v1 and v1beta1 are different objects), and a defaulted `resource` streams the wrong
	// objects entirely. Both must be said out loud.
	if s.Resource == "" {
		return Scope{}, ScopeInvalid("scope needs a `resource` (the plural, lowercase API name, e.g. `configmaps`)")
	}
	if s.Version == "" {
		return Scope{}, ScopeInvalid("scope needs a `version` (e.g. `v1`): there is no default, because `v1` and `v1beta1` are different objects")
	}

	// `resource` is the API's plural name, not the Kind. Getting this wrong is the single most common
	// first-day mistake, and it must fail HERE with an explanation rather than as a 404 from the API
	// server three layers down.
	if s.Resource != strings.ToLower(s.Resource) {
		return Scope{}, ScopeInvalid("`resource` is the plural, lowercase API name (`configmaps`), not the Kind (`ConfigMap`)")
	}
	if !isDNSName(s.Resource) {
		return Scope{}, ScopeInvalid("`resource` is not a valid API resource name: " + s.Resource)
	}
	if s.Group != "" && !isDNSName(s.Group) {
		return Scope{}, ScopeInvalid("`group` is a DNS subdomain (`apps`, `wardle.example.com`), not a path: " + s.Group)
	}
	if !isDNSName(s.Version) {
		return Scope{}, ScopeInvalid("`version` is not a valid API version: " + s.Version)
	}
	if s.Namespace != "" && !isDNSName(s.Namespace) {
		return Scope{}, ScopeInvalid("`namespace` is not a valid namespace name: " + s.Namespace)
	}
	if s.Name != "" && !isDNSName(s.Name) {
		return Scope{}, ScopeInvalid("`name` is not a valid object name: " + s.Name)
	}
	if s.Target != "" && !isDNSName(s.Target) {
		return Scope{}, ScopeInvalid("`target` is a logical name the host resolves, not an address: " + s.Target)
	}
	return s, nil
}

// Query renders a scope as the CANONICAL query string — one fixed field order, so a URL is stable,
// cacheable and diffable, and so the client's output is byte-comparable with what this parser
// accepts. It is the Go twin of the client's resourceStreamURL(), and conformance/scopes.yaml is
// what keeps the two honest.
func (s Scope) Query() url.Values {
	q := url.Values{}
	set := func(k, v string) {
		if v != "" {
			q.Set(k, v)
		}
	}
	set("target", s.Target)
	set("group", s.Group)
	set("version", s.Version)
	set("resource", s.Resource)
	set("namespace", s.Namespace)
	set("name", s.Name)
	set("labelSelector", s.LabelSelector)
	return q
}

// isDNSName accepts the shape every one of these fields actually has in Kubernetes: a DNS subdomain.
// Lowercase alphanumerics, '-' and '.', starting and ending alphanumeric. It exists so that a value
// like `../../kube-system` is rejected AT THE EDGE rather than carried around inside a Scope that
// everything downstream is entitled to assume was validated.
func isDNSName(v string) bool {
	if v == "" || len(v) > 253 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' || c == '.':
			// A separator may not lead or trail: `.foo` and `foo-` are not names.
			if i == 0 || i == len(v)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// ResourceScope states the Kubernetes scope of a resource in the host allowlist.
type ResourceScope string

const (
	// ResourceScopeNamespaced is a resource such as ConfigMap or Deployment.
	ResourceScopeNamespaced ResourceScope = "namespaced"
	// ResourceScopeCluster is a resource such as Namespace or Node.
	ResourceScopeCluster ResourceScope = "cluster"
)

// GroupResource names one kind of thing a gateway is willing to stream. Group is "" for the core
// group ("" + "configmaps"), exactly as Kubernetes writes it.
type GroupResource struct {
	Group              string
	Resource           string
	Scope              ResourceScope
	AllowAllNamespaces bool
}

// ScopePolicy is the allowlist spec §8 demands: "the scope is allowlisted and server-normalized".
//
// It is DENY BY DEFAULT, and that is not a style choice. The zero value permits nothing, so a host
// that forgets to configure it serves nothing — rather than serving Secrets from every namespace in
// every cluster it can reach, which is what the other default would have done the first time someone
// copy-pasted the README.
//
// It answers "may ANYONE stream this kind of thing here?". It does NOT answer "may THIS caller see
// it" — that is the Authorizer, per principal, and no allowlist can stand in for it.
type ScopePolicy struct {
	// Targets are the logical upstream names a caller may ask for. The empty string is a valid
	// target name (a single-cluster host that never sets one), so include "" if you want it.
	Targets []string
	// Resources are the group+resource pairs that may be streamed.
	Resources []GroupResource
	// AllowLabelSelector permits a caller to narrow an already-allowed scope with Kubernetes label
	// selector syntax. The zero value refuses selectors rather than silently expanding the supported
	// request surface. A host that enables it should still constrain selector complexity at its edge.
	AllowLabelSelector bool
}

// Validate reports whether a scope is one this gateway will stream at all.
//
// # Why this returns `error` and not `*StreamError`
//
// Because returning a concrete pointer type from a fallible exported function is the Go typed-nil
// trap, and this one is a security control. A caller writing the ordinary thing:
//
//	var err error          // …or an err already in scope from something else
//	if err = policy.Validate(scope); err != nil {
//		return err     // FIRES ON SUCCESS. Always.
//	}
//
// gets a non-nil `error` holding a nil *StreamError, because an interface holding a typed nil is not
// nil. Validate said yes and the caller read no. That version fails safe, and it is the lucky one: an
// authorizer written the same way refuses everyone and someone notices in a minute. Invert the check —
// `if err == nil { serve() }` — and the same bug admits everyone, silently, and nobody notices at all.
//
// So the exported surface speaks `error`, which callers cannot get wrong. The concrete value is still
// a *StreamError, and the gateway still switches on it internally; a host that wants the wire code
// asks for it explicitly:
//
//	var se *gateway.StreamError
//	if errors.As(err, &se) { … se.Code … }
func (p ScopePolicy) Validate(s Scope) error {
	if s.LabelSelector != "" && !p.AllowLabelSelector {
		return ScopeInvalid("label selectors are not enabled for this stream endpoint")
	}
	if !contains(p.Targets, s.Target) {
		return ScopeInvalid("target is not allowlisted: " + quoteOrEmpty(s.Target))
	}
	for _, gr := range p.Resources {
		if gr.Group != s.Group || gr.Resource != s.Resource {
			continue
		}
		switch gr.Scope {
		case ResourceScopeCluster:
			if s.Namespace != "" {
				return ScopeInvalid("cluster-scoped resource must not name a namespace: " + quoteOrEmpty(groupResourceOf(s)))
			}
			return nil
		case ResourceScopeNamespaced:
			if s.Namespace != "" || gr.AllowAllNamespaces {
				return nil
			}
			return ScopeInvalid("all-namespaces watch is not allowlisted for: " + quoteOrEmpty(groupResourceOf(s)))
		default:
			return ScopeInvalid("resource scope is not configured for: " + quoteOrEmpty(groupResourceOf(s)))
		}
	}
	// Deliberately does NOT echo back which resources are allowed: the allowlist is a policy
	// statement, and enumerating it for an unauthenticated caller is a free reconnaissance report.
	return ScopeInvalid("resource is not allowlisted: " + quoteOrEmpty(groupResourceOf(s)))
}

func groupResourceOf(s Scope) string {
	if s.Group == "" {
		return s.Resource
	}
	return s.Group + "/" + s.Resource
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func quoteOrEmpty(v string) string {
	if v == "" {
		return `""`
	}
	return v
}
