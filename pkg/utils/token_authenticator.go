package utils

import (
	"context"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/token/cache"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	// DefaultTokenReviewCacheTTL deduplicates concurrent TokenReviews for the same
	// bearer token while still picking up token revocation quickly.
	DefaultTokenReviewCacheTTL = 10 * time.Second

	// DefaultKubeClientQPS and DefaultKubeClientBurst raise the client-go defaults
	// (5/10), which are too low for the high-concurrency TokenReview path.
	DefaultKubeClientQPS   = 50.0
	DefaultKubeClientBurst = 100
)

// NewCachedTokenAuthenticator returns a TokenReview-backed authenticator for the
// named cluster. When cacheTTL > 0, results (including unauthenticated ones) are
// cached for cacheTTL so concurrent requests for the same token coalesce into a
// single TokenReview call, matching the kube-apiserver delegating authenticator;
// transient errors are never cached. A cacheTTL <= 0 disables caching.
func NewCachedTokenAuthenticator(client kubernetes.Interface, name string, cacheTTL time.Duration) authenticator.Token {
	var authn authenticator.Token = &TokenReviewAuthenticator{Client: client, Name: name}
	if cacheTTL > 0 {
		authn = cache.New(authn, false, cacheTTL, cacheTTL)
	}
	return authn
}

// TokenReviewAuthenticator implements authenticator.Token by calling the
// Kubernetes TokenReview API against a specific cluster.
type TokenReviewAuthenticator struct {
	Client kubernetes.Interface
	Name   string // cluster name for logging (e.g., "managed cluster", "hub")
}

// AuthenticateToken calls the TokenReview API and returns the result.
func (a *TokenReviewAuthenticator) AuthenticateToken(ctx context.Context, token string) (*authenticator.Response, bool, error) {
	logger := klog.FromContext(ctx)
	logger.V(6).Info("creating TokenReview", "cluster", a.Name)

	tokenReview, err := a.Client.AuthenticationV1().TokenReviews().Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{
			Token: token,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return nil, false, err
	}

	logger.V(6).Info("TokenReview completed",
		"cluster", a.Name,
		"authenticated", tokenReview.Status.Authenticated,
		"username", tokenReview.Status.User.Username,
		"groups", tokenReview.Status.User.Groups,
	)

	// A completed TokenReview is this cluster's definitive answer. If it did not
	// authenticate the token, report it as unauthenticated so the caller can try
	// another cluster; Status.Error, when set, only explains the rejection.
	if !tokenReview.Status.Authenticated {
		if tokenReview.Status.Error != "" {
			logger.V(4).Info("TokenReview rejected token", "cluster", a.Name, "error", tokenReview.Status.Error)
		}
		return nil, false, nil
	}

	return &authenticator.Response{
		User: &user.DefaultInfo{
			Name:   tokenReview.Status.User.Username,
			UID:    tokenReview.Status.User.UID,
			Groups: tokenReview.Status.User.Groups,
			Extra:  convertExtra(tokenReview.Status.User.Extra),
		},
	}, true, nil
}

// convertExtra converts authenticationv1.ExtraValue (map[string]ExtraValue)
// to the format expected by user.Info (map[string][]string).
func convertExtra(extra map[string]authenticationv1.ExtraValue) map[string][]string {
	if extra == nil {
		return nil
	}
	result := make(map[string][]string, len(extra))
	for k, v := range extra {
		result[k] = []string(v)
	}
	return result
}
