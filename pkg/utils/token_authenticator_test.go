package utils

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// newFakeClient creates a fake kubernetes client that responds to TokenReview
// requests with the given authenticated status and user info.
func newFakeClient(authenticated bool, username string, groups []string) *fake.Clientset {
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		tr := &authenticationv1.TokenReview{
			Status: authenticationv1.TokenReviewStatus{
				Authenticated: authenticated,
				User: authenticationv1.UserInfo{
					Username: username,
					Groups:   groups,
				},
			},
		}
		return true, tr, nil
	})
	return client
}

func TestTokenReviewAuthenticator_Authenticated(t *testing.T) {
	client := newFakeClient(true, "system:serviceaccount:ns:sa", []string{"system:authenticated"})
	authn := &TokenReviewAuthenticator{Client: client, Name: "test"}

	resp, ok, err := authn.AuthenticateToken(context.Background(), "test-token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected authenticated=true")
	}
	if resp.User.GetName() != "system:serviceaccount:ns:sa" {
		t.Fatalf("expected username 'system:serviceaccount:ns:sa', got '%s'", resp.User.GetName())
	}
	if len(resp.User.GetGroups()) != 1 || resp.User.GetGroups()[0] != "system:authenticated" {
		t.Fatalf("unexpected groups: %v", resp.User.GetGroups())
	}
}

func TestTokenReviewAuthenticator_Unauthenticated(t *testing.T) {
	client := newFakeClient(false, "", nil)
	authn := &TokenReviewAuthenticator{Client: client, Name: "test"}

	resp, ok, err := authn.AuthenticateToken(context.Background(), "bad-token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("expected authenticated=false")
	}
	if resp != nil {
		t.Fatal("expected nil response for unauthenticated token")
	}
}

func TestNewCachedTokenAuthenticator(t *testing.T) {
	tests := []struct {
		name          string
		authenticated bool
		cacheTTL      time.Duration
		requests      int
		wantOK        bool
		wantCalls     int32
	}{
		{name: "repeated authenticated token hits cache", authenticated: true, cacheTTL: time.Minute, requests: 5, wantOK: true, wantCalls: 1},
		{name: "unauthenticated results are cached too", authenticated: false, cacheTTL: time.Minute, requests: 4, wantOK: false, wantCalls: 1},
		{name: "disabling the cache reviews every request", authenticated: true, cacheTTL: 0, requests: 3, wantOK: true, wantCalls: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls int32
			client := fake.NewSimpleClientset()
			client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
				atomic.AddInt32(&calls, 1)
				return true, &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{
					Authenticated: tt.authenticated,
					User:          authenticationv1.UserInfo{Username: "system:serviceaccount:ns:sa"},
				}}, nil
			})
			authn := NewCachedTokenAuthenticator(client, "test", tt.cacheTTL)

			for i := 0; i < tt.requests; i++ {
				_, ok, err := authn.AuthenticateToken(context.Background(), "cached-token")
				if err != nil {
					t.Fatalf("call %d: unexpected error: %v", i, err)
				}
				if ok != tt.wantOK {
					t.Fatalf("call %d: authenticated = %v, want %v", i, ok, tt.wantOK)
				}
			}
			if got := atomic.LoadInt32(&calls); got != tt.wantCalls {
				t.Fatalf("expected %d TokenReview call(s), got %d", tt.wantCalls, got)
			}
		})
	}
}

func TestConvertExtra(t *testing.T) {
	extra := map[string]authenticationv1.ExtraValue{
		"example.org/scope": {"read", "write"},
	}
	result := convertExtra(extra)
	if len(result) != 1 {
		t.Fatalf("expected 1 key, got %d", len(result))
	}
	if len(result["example.org/scope"]) != 2 {
		t.Fatalf("expected 2 values, got %d", len(result["example.org/scope"]))
	}

	if convertExtra(nil) != nil {
		t.Fatal("expected nil for nil input")
	}
}

func TestTokenReviewAuthenticator_TokenSentInRequest(t *testing.T) {
	var capturedToken string
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction := action.(k8stesting.CreateAction)
		tr := createAction.GetObject().(*authenticationv1.TokenReview)
		capturedToken = tr.Spec.Token
		return true, &authenticationv1.TokenReview{
			ObjectMeta: metav1.ObjectMeta{Name: "test"},
			Status:     authenticationv1.TokenReviewStatus{Authenticated: false},
		}, nil
	})

	authn := &TokenReviewAuthenticator{Client: client, Name: "test"}
	authn.AuthenticateToken(context.Background(), "my-secret-token")

	if capturedToken != "my-secret-token" {
		t.Fatalf("expected token 'my-secret-token' to be sent in TokenReview, got '%s'", capturedToken)
	}
}

func TestTokenReviewAuthenticator_APIError(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("connection refused")
	})

	authn := &TokenReviewAuthenticator{Client: client, Name: "test"}
	resp, ok, err := authn.AuthenticateToken(context.Background(), "some-token")
	if err == nil {
		t.Fatal("expected error from API call")
	}
	if ok {
		t.Fatal("expected authenticated=false on API error")
	}
	if resp != nil {
		t.Fatal("expected nil response on API error")
	}
}

func TestTokenReviewAuthenticator_StatusError_ReturnsUnauthenticated(t *testing.T) {
	// An unauthenticated TokenReview is reported as (nil, false, nil) regardless
	// of how Status.Error is worded, so the caller can fall back to another cluster.
	tests := []struct {
		name        string
		statusError string
	}{
		{name: "Kubernetes: invalid bearer token", statusError: "invalid bearer token"},
		{name: "OpenShift: token lookup failed", statusError: "[invalid bearer token, token lookup failed]"},
		{name: "Kubernetes: expired credentials", statusError: "Credentials are expired"},
		{name: "webhook backend error", statusError: "webhook authenticator connection reset"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
				return true, &authenticationv1.TokenReview{
					Status: authenticationv1.TokenReviewStatus{
						Authenticated: false,
						Error:         tt.statusError,
					},
				}, nil
			})

			authn := &TokenReviewAuthenticator{Client: client, Name: "test"}
			resp, ok, err := authn.AuthenticateToken(context.Background(), "bad-token")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ok {
				t.Fatal("expected authenticated=false")
			}
			if resp != nil {
				t.Fatal("expected nil response")
			}
		})
	}
}
