package serviceproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestExtractClientImpersonation(t *testing.T) {
	tests := []struct {
		name       string
		headers    http.Header
		want       *impersonationRequest
		wantStatus int
	}{
		{
			name: "all supported headers",
			headers: http.Header{
				"impersonate-user":                      {"target"},
				"IMPERSONATE-GROUP":                     {"group-a", "group-b"},
				"Impersonate-Uid":                       {"target-uid"},
				"Impersonate-Extra-Example.org%2FScope": {"read", "write"},
				"Impersonate-Extra-Scopes.Example.org":  {"admin"},
				"X-Unrelated":                           {"preserved"},
			},
			want: &impersonationRequest{
				user:   "target",
				groups: []string{"group-a", "group-b"},
				uid:    "target-uid",
				extra: map[string][]string{
					"example.org/scope":  {"read", "write"},
					"scopes.example.org": {"admin"},
				},
			},
		},
		{
			name: "group without user",
			headers: http.Header{
				"Impersonate-Group": {"group-a"},
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "duplicate user",
			headers: http.Header{
				"Impersonate-User": {"target-a", "target-b"},
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "unknown impersonation header",
			headers: http.Header{
				"Impersonate-User":   {"target"},
				"Impersonate-Future": {"value"},
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "reserved audit extra",
			headers: http.Header{
				"Impersonate-User": {"target"},
				authenticationv1.ImpersonateUserExtraHeaderPrefix + originalUserExtraUser: {"spoofed"},
			},
			wantStatus: http.StatusForbidden,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := extractClientImpersonation(test.headers)
			if test.wantStatus == 0 {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if !reflect.DeepEqual(got, test.want) {
					t.Fatalf("unexpected request:\nwant: %#v\ngot:  %#v", test.want, got)
				}
			} else {
				assertRequestErrorStatus(t, err, test.wantStatus)
			}

			for key := range test.headers {
				if strings.HasPrefix(strings.ToLower(key), "impersonate-") {
					t.Fatalf("untrusted header %q was not removed", key)
				}
			}
			if values, ok := test.headers["X-Unrelated"]; ok && !reflect.DeepEqual(values, []string{"preserved"}) {
				t.Fatalf("unrelated header was changed: %v", values)
			}
		})
	}
}

func TestApplyImpersonationHeadersRebuildsTrustedValues(t *testing.T) {
	headers := http.Header{
		"Impersonate-Future": {"untrusted"},
		"X-Unrelated":        {"preserved"},
	}
	requested := &impersonationRequest{
		user:   "target",
		groups: []string{"group-a", "group-b"},
		uid:    "uid-a",
		extra: map[string][]string{
			"example.org/scope": {"read", "write"},
		},
	}

	applyImpersonationHeaders(headers, requested)

	if got := headers.Get(authenticationv1.ImpersonateUserHeader); got != "target" {
		t.Fatalf("unexpected user: %q", got)
	}
	if got := headers.Values(authenticationv1.ImpersonateGroupHeader); !reflect.DeepEqual(got, []string{"group-a", "group-b"}) {
		t.Fatalf("unexpected groups: %v", got)
	}
	if got := headers.Get(authenticationv1.ImpersonateUIDHeader); got != "uid-a" {
		t.Fatalf("unexpected UID: %q", got)
	}
	if got := headers.Values(authenticationv1.ImpersonateUserExtraHeaderPrefix + "example.org%2Fscope"); !reflect.DeepEqual(got, []string{"read", "write"}) {
		t.Fatalf("unexpected extra: %v", got)
	}
	if _, ok := headers["Impersonate-Future"]; ok {
		t.Fatal("unknown untrusted impersonation header was preserved")
	}
	if got := headers.Get("X-Unrelated"); got != "preserved" {
		t.Fatalf("unrelated header was changed: %q", got)
	}
}

func TestAuthorizeImpersonationBuildsKubernetesSubjectAccessReviews(t *testing.T) {
	var reviews []*authorizationv1.SubjectAccessReview
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview).DeepCopy()
		reviews = append(reviews, review)
		review.Status.Allowed = true
		return true, review, nil
	})

	s := &serviceProxy{managedClusterKubeClient: client}
	requester := &user.DefaultInfo{
		Name:   "requester",
		UID:    "requester-uid",
		Groups: []string{"group-a"},
		Extra:  map[string][]string{"source": {"hub"}},
	}
	requested := &impersonationRequest{
		user:   "system:serviceaccount:target-ns:target-sa",
		groups: []string{"target-group"},
		uid:    "target-uid",
		extra: map[string][]string{
			"example.org/scope": {"read", "write"},
		},
	}

	if err := s.authorizeImpersonation(t.Context(), requester, requested); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantAttributes := []authorizationv1.ResourceAttributes{
		{Namespace: "target-ns", Verb: "impersonate", Resource: "serviceaccounts", Name: "target-sa"},
		{Verb: "impersonate", Resource: "groups", Name: "target-group"},
		{Verb: "impersonate", Group: authenticationv1.GroupName, Version: "v1", Resource: "uids", Name: "target-uid"},
		{Verb: "impersonate", Group: authenticationv1.GroupName, Version: "v1", Resource: "userextras", Subresource: "example.org/scope", Name: "read"},
		{Verb: "impersonate", Group: authenticationv1.GroupName, Version: "v1", Resource: "userextras", Subresource: "example.org/scope", Name: "write"},
	}
	if len(reviews) != len(wantAttributes) {
		t.Fatalf("expected %d reviews, got %d", len(wantAttributes), len(reviews))
	}
	for i, review := range reviews {
		if !reflect.DeepEqual(*review.Spec.ResourceAttributes, wantAttributes[i]) {
			t.Fatalf("review %d attributes:\nwant: %#v\ngot:  %#v", i, wantAttributes[i], *review.Spec.ResourceAttributes)
		}
		if review.Spec.User != "requester" ||
			review.Spec.UID != "requester-uid" ||
			!reflect.DeepEqual(review.Spec.Groups, []string{"group-a"}) ||
			!reflect.DeepEqual(review.Spec.Extra, map[string]authorizationv1.ExtraValue{"source": {"hub"}}) {
			t.Fatalf("review %d requester identity was not preserved: %#v", i, review.Spec)
		}
	}
}

func TestAuthorizeImpersonationFailureModes(t *testing.T) {
	tests := []struct {
		name       string
		status     authorizationv1.SubjectAccessReviewStatus
		apiErr     error
		wantStatus int
	}{
		{
			name: "denied",
			status: authorizationv1.SubjectAccessReviewStatus{
				Denied: true,
				Reason: "RBAC deny",
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "no opinion",
			status:     authorizationv1.SubjectAccessReviewStatus{},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "evaluation error",
			status: authorizationv1.SubjectAccessReviewStatus{
				EvaluationError: "webhook unavailable",
			},
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:       "API error",
			apiErr:     errors.New("connection refused"),
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			client.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
				if test.apiErr != nil {
					return true, nil, test.apiErr
				}
				review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview).DeepCopy()
				review.Status = test.status
				return true, review, nil
			})
			s := &serviceProxy{managedClusterKubeClient: client}

			err := s.authorizeImpersonation(
				t.Context(),
				&user.DefaultInfo{Name: "requester"},
				&impersonationRequest{user: "target"},
			)
			assertRequestErrorStatus(t, err, test.wantStatus)
		})
	}
}

func TestProcessAuthenticationHubDelegatedImpersonation(t *testing.T) {
	client := newFakeImpersonationClient(true)
	s := &serviceProxy{
		managedClusterKubeClient: client,
		managedClusterAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return nil, false, nil
		}),
		hubAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return &authenticator.Response{User: &user.DefaultInfo{
				Name:   "alice",
				UID:    "alice-uid",
				Groups: []string{"developers", "system:authenticated"},
				Extra:  map[string][]string{"issuer": {"hub"}},
			}}, true, nil
		}),
		getImpersonateTokenFunc: func() (string, error) {
			return "proxy-sa-token", nil
		},
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/api", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer hub-token")
	req.Header.Set(authenticationv1.ImpersonateUserHeader, "bob")
	req.Header.Add(authenticationv1.ImpersonateGroupHeader, "operators")
	req.Header.Set(authenticationv1.ImpersonateUIDHeader, "bob-uid")
	req.Header.Add(authenticationv1.ImpersonateUserExtraHeaderPrefix+"example.org%2Fscope", "write")

	if err := s.processAuthentication(t.Context(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := req.Header.Get("Authorization"); got != "Bearer proxy-sa-token" {
		t.Fatalf("unexpected authorization header: %q", got)
	}
	if got := req.Header.Get(authenticationv1.ImpersonateUserHeader); got != "bob" {
		t.Fatalf("unexpected target user: %q", got)
	}
	if got := req.Header.Values(authenticationv1.ImpersonateGroupHeader); !reflect.DeepEqual(got, []string{"operators"}) {
		t.Fatalf("unexpected target groups: %v", got)
	}
	if got := req.Header.Get(authenticationv1.ImpersonateUIDHeader); got != "bob-uid" {
		t.Fatalf("unexpected target UID: %q", got)
	}
	if got := req.Header.Get(authenticationv1.ImpersonateUserExtraHeaderPrefix + originalUserExtraUser); got != "alice" {
		t.Fatalf("original user audit extra missing: %q", got)
	}
	if got := req.Header.Values(authenticationv1.ImpersonateUserExtraHeaderPrefix + originalUserExtraGroups); !reflect.DeepEqual(got, []string{"developers", "system:authenticated"}) {
		t.Fatalf("original groups audit extra missing: %v", got)
	}
	if got := req.Header.Get(authenticationv1.ImpersonateUserExtraHeaderPrefix + originalUserExtraUID); got != "alice-uid" {
		t.Fatalf("original UID audit extra missing: %q", got)
	}
	var originalExtra map[string][]string
	if err := json.Unmarshal(
		[]byte(req.Header.Get(authenticationv1.ImpersonateUserExtraHeaderPrefix+originalUserExtraExtra)),
		&originalExtra,
	); err != nil {
		t.Fatalf("invalid original extra audit value: %v", err)
	}
	if !reflect.DeepEqual(originalExtra, map[string][]string{"issuer": {"hub"}}) {
		t.Fatalf("unexpected original extra audit value: %#v", originalExtra)
	}
}

func TestProcessAuthenticationHubServiceAccountUsesMappedRequesterForSAR(t *testing.T) {
	var sarRequester string
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview).DeepCopy()
		sarRequester = review.Spec.User
		review.Status.Allowed = true
		return true, review, nil
	})
	s := &serviceProxy{
		managedClusterKubeClient: client,
		managedClusterAuthenticator: authenticator.TokenFunc(func(context.Context, string) (*authenticator.Response, bool, error) {
			return nil, false, nil
		}),
		hubAuthenticator: authenticator.TokenFunc(func(context.Context, string) (*authenticator.Response, bool, error) {
			return &authenticator.Response{User: &user.DefaultInfo{
				Name: "system:serviceaccount:hub-ns:hub-sa",
			}}, true, nil
		}),
		getImpersonateTokenFunc: func() (string, error) {
			return "proxy-sa-token", nil
		},
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/api", nil)
	req.Header.Set(authenticationv1.ImpersonateUserHeader, "target")

	if err := s.processAuthentication(t.Context(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sarRequester != "cluster:hub:system:serviceaccount:hub-ns:hub-sa" {
		t.Fatalf("unexpected SAR requester: %q", sarRequester)
	}
	if got := req.Header.Get(authenticationv1.ImpersonateUserExtraHeaderPrefix + originalUserExtraUser); got != sarRequester {
		t.Fatalf("unexpected original-user audit value: %q", got)
	}
}

func TestMalformedImpersonationStillAuthenticatesFirst(t *testing.T) {
	authenticated := false
	s := &serviceProxy{
		managedClusterAuthenticator: authenticator.TokenFunc(func(context.Context, string) (*authenticator.Response, bool, error) {
			authenticated = true
			return nil, false, nil
		}),
		hubAuthenticator: authenticator.TokenFunc(func(context.Context, string) (*authenticator.Response, bool, error) {
			return nil, false, nil
		}),
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/api", nil)
	req.Header.Set(authenticationv1.ImpersonateGroupHeader, "group-without-user")

	err := s.processAuthentication(t.Context(), req)
	if !authenticated {
		t.Fatal("request was rejected before authentication")
	}
	assertRequestErrorStatus(t, err, http.StatusUnauthorized)
	assertNoImpersonationHeaders(t, req.Header)
}

func TestWriteRequestProcessingErrorUsesKubernetesStatus(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeRequestProcessingError(
		recorder,
		newRequestProcessingError(http.StatusForbidden, "impersonation denied", fmt.Errorf("internal detail")),
	)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", recorder.Code)
	}
	var status metav1.Status
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatalf("invalid status response: %v", err)
	}
	if status.Status != metav1.StatusFailure ||
		status.Reason != metav1.StatusReasonForbidden ||
		status.Message != "impersonation denied" ||
		status.Code != http.StatusForbidden {
		t.Fatalf("unexpected status response: %#v", status)
	}
}

func TestServeHTTPImpersonationStatusCodes(t *testing.T) {
	tests := []struct {
		name           string
		headers        http.Header
		managedAuth    bool
		hubAuth        bool
		sarStatus      authorizationv1.SubjectAccessReviewStatus
		sarErr         error
		expectedCode   int
		expectedReason metav1.StatusReason
	}{
		{
			name: "malformed request",
			headers: http.Header{
				authenticationv1.ImpersonateGroupHeader: {"group-without-user"},
			},
			managedAuth:    true,
			expectedCode:   http.StatusBadRequest,
			expectedReason: metav1.StatusReasonBadRequest,
		},
		{
			name: "unauthenticated request",
			headers: http.Header{
				authenticationv1.ImpersonateGroupHeader: {"group-without-user"},
			},
			expectedCode:   http.StatusUnauthorized,
			expectedReason: metav1.StatusReasonUnauthorized,
		},
		{
			name: "denied impersonation",
			headers: http.Header{
				authenticationv1.ImpersonateUserHeader: {"target"},
			},
			managedAuth: true,
			sarStatus: authorizationv1.SubjectAccessReviewStatus{
				Denied: true,
			},
			expectedCode:   http.StatusForbidden,
			expectedReason: metav1.StatusReasonForbidden,
		},
		{
			name: "SAR API error",
			headers: http.Header{
				authenticationv1.ImpersonateUserHeader: {"target"},
			},
			managedAuth:    true,
			sarErr:         errors.New("authorization API unavailable"),
			expectedCode:   http.StatusInternalServerError,
			expectedReason: metav1.StatusReasonInternalError,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			client.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
				if test.sarErr != nil {
					return true, nil, test.sarErr
				}
				review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview).DeepCopy()
				review.Status = test.sarStatus
				return true, review, nil
			})
			s := &serviceProxy{
				enableImpersonation:      true,
				managedClusterKubeClient: client,
				managedClusterAuthenticator: authenticator.TokenFunc(func(context.Context, string) (*authenticator.Response, bool, error) {
					if !test.managedAuth {
						return nil, false, nil
					}
					return &authenticator.Response{User: &user.DefaultInfo{Name: "requester"}}, true, nil
				}),
				hubAuthenticator: authenticator.TokenFunc(func(context.Context, string) (*authenticator.Response, bool, error) {
					if !test.hubAuth {
						return nil, false, nil
					}
					return &authenticator.Response{User: &user.DefaultInfo{Name: "hub-requester"}}, true, nil
				}),
			}
			req := httptest.NewRequest(http.MethodGet, "https://service-proxy.example/api", nil)
			req.Header = test.headers.Clone()
			req.Header.Set("Cluster-Proxy-Proto", "https")
			req.Header.Set("Cluster-Proxy-Namespace", "default")
			req.Header.Set("Cluster-Proxy-Service", "kubernetes")
			req.Header.Set("Cluster-Proxy-Port", "443")
			recorder := httptest.NewRecorder()

			s.ServeHTTP(recorder, req)

			if recorder.Code != test.expectedCode {
				t.Fatalf("expected status %d, got %d: %s", test.expectedCode, recorder.Code, recorder.Body.String())
			}
			var status metav1.Status
			if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
				t.Fatalf("invalid Kubernetes status: %v", err)
			}
			if status.Reason != test.expectedReason {
				t.Fatalf("expected reason %q, got %q", test.expectedReason, status.Reason)
			}
		})
	}
}

func assertRequestErrorStatus(t *testing.T, err error, want int) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected request error with status %d", want)
	}
	var processingErr *requestProcessingError
	if !errors.As(err, &processingErr) {
		t.Fatalf("expected requestProcessingError, got %T: %v", err, err)
	}
	if processingErr.statusCode != want {
		t.Fatalf("expected status %d, got %d: %v", want, processingErr.statusCode, err)
	}
}
