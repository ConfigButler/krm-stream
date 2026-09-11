package sharedstream

import (
	"context"
	"fmt"
	"slices"

	"github.com/ConfigButler/krm-stream/gateway/kube"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// The supplied client MUST authenticate as the participant. A service-account
// client resolves that service account instead. This is neither token refresh nor
// an authorization grant; the host chooses when the captured identity expires.
func resolveSubject(ctx context.Context, participant kubernetes.Interface) (kube.Subject, error) {
	result, err := participant.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		return kube.Subject{}, fmt.Errorf("resolve Kubernetes subject: %w", err)
	}
	if result == nil || result.Status.UserInfo.Username == "" {
		return kube.Subject{}, fmt.Errorf("SelfSubjectReview returned no username")
	}
	info := result.Status.UserInfo
	subject := kube.Subject{User: info.Username, UID: info.UID, Groups: slices.Clone(info.Groups)}
	if info.Extra != nil {
		subject.Extra = make(map[string]authorizationv1.ExtraValue, len(info.Extra))
		for key, values := range info.Extra {
			subject.Extra[key] = slices.Clone(authorizationv1.ExtraValue(values))
		}
	}
	return subject, nil
}
