package serviceproxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/client-go/rest"

	"open-cluster-management.io/cluster-proxy/pkg/utils"
)

func TestProcessAuthentication_ManagedClusterToken(t *testing.T) {
	s := &serviceProxy{
		enableImpersonation: true,
		managedClusterAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return &authenticator.Response{User: &user.DefaultInfo{Name: "mc-user"}}, true, nil
		}),
		hubAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			t.Fatal("hub authenticator should not be called for managed cluster token")
			return nil, false, nil
		}),
	}

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer mc-token")

	if err := s.processAuthentication(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// For managed cluster tokens, no impersonation headers should be set
	if req.Header.Get("Impersonate-User") != "" {
		t.Fatal("impersonation headers should not be set for managed cluster token")
	}
}

func TestProcessAuthentication_HubServiceAccountToken(t *testing.T) {
	s := &serviceProxy{
		enableImpersonation: true,
		managedClusterAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return nil, false, nil // not a managed cluster token
		}),
		hubAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return &authenticator.Response{
				User: &user.DefaultInfo{
					Name:   "system:serviceaccount:ns:my-sa",
					Groups: []string{"system:serviceaccounts", "system:authenticated"},
				},
			}, true, nil
		}),
		getImpersonateTokenFunc: func() (string, error) {
			return "fake-sa-token", nil
		},
	}

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer hub-token")

	err := s.processAuthentication(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify impersonation headers were set
	if req.Header.Get("Impersonate-User") != "cluster:hub:system:serviceaccount:ns:my-sa" {
		t.Fatalf("expected impersonate user with cluster:hub: prefix, got '%s'", req.Header.Get("Impersonate-User"))
	}

	// Verify the original token was replaced with the impersonation token
	if req.Header.Get("Authorization") != "Bearer fake-sa-token" {
		t.Fatalf("expected authorization header to use impersonation token, got '%s'", req.Header.Get("Authorization"))
	}
}

func TestProcessAuthentication_UnauthenticatedToken(t *testing.T) {
	s := &serviceProxy{
		enableImpersonation: true,
		managedClusterAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return nil, false, nil
		}),
		hubAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return nil, false, nil
		}),
	}

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer invalid-token")

	err := s.processAuthentication(context.Background(), req)
	if err == nil {
		t.Fatal("expected authentication error")
	}
	if !strings.Contains(err.Error(), "neither valid for managed cluster nor hub cluster") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProcessHubUser_RegularUser(t *testing.T) {
	s := &serviceProxy{
		getImpersonateTokenFunc: func() (string, error) {
			return "fake-sa-token", nil
		},
	}
	req, _ := http.NewRequest("GET", "https://example.com/api", nil)

	hubUser := &user.DefaultInfo{
		Name:   "admin@example.com",
		Groups: []string{"system:authenticated", "admins"},
	}

	if err := s.processHubUser(context.Background(), req, hubUser); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Regular user should NOT get cluster:hub: prefix
	if req.Header.Get("Impersonate-User") != "admin@example.com" {
		t.Fatalf("expected impersonate user 'admin@example.com', got '%s'", req.Header.Get("Impersonate-User"))
	}

	groups := req.Header.Values("Impersonate-Group")
	if len(groups) != 2 {
		t.Fatalf("expected 2 impersonate groups, got %d: %v", len(groups), groups)
	}
}

func TestProcessHubUser_ServiceAccount(t *testing.T) {
	s := &serviceProxy{
		getImpersonateTokenFunc: func() (string, error) {
			return "fake-sa-token", nil
		},
	}
	req, _ := http.NewRequest("GET", "https://example.com/api", nil)

	hubUser := &user.DefaultInfo{
		Name:   "system:serviceaccount:proxy-test:proxy-bench",
		Groups: []string{"system:serviceaccounts", "system:authenticated"},
	}

	if err := s.processHubUser(context.Background(), req, hubUser); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := "cluster:hub:system:serviceaccount:proxy-test:proxy-bench"
	if req.Header.Get("Impersonate-User") != expected {
		t.Fatalf("expected impersonate user '%s', got '%s'", expected, req.Header.Get("Impersonate-User"))
	}
}

func TestNewServiceProxy_DefaultValues(t *testing.T) {
	s := newServiceProxy()

	if s.tokenReviewCacheTTL != utils.DefaultTokenReviewCacheTTL {
		t.Fatalf("expected default TTL %v, got %v", utils.DefaultTokenReviewCacheTTL, s.tokenReviewCacheTTL)
	}
	if s.kubeClientQPS != utils.DefaultKubeClientQPS {
		t.Fatalf("expected default QPS %v, got %v", utils.DefaultKubeClientQPS, s.kubeClientQPS)
	}
	if s.kubeClientBurst != utils.DefaultKubeClientBurst {
		t.Fatalf("expected default burst %v, got %v", utils.DefaultKubeClientBurst, s.kubeClientBurst)
	}
}

func TestManagedKubeconfigConfigAndToken(t *testing.T) {
	kubeconfig := `apiVersion: v1
kind: Config
clusters:
- name: managed
  cluster:
    server: https://managed.example.com:6443
contexts:
- name: managed
  context:
    cluster: managed
    user: cluster-proxy
current-context: managed
users:
- name: cluster-proxy
  user:
    token: managed-token
`
	path := t.TempDir() + "/kubeconfig"
	if err := os.WriteFile(path, []byte(kubeconfig), 0600); err != nil {
		t.Fatalf("failed to write kubeconfig: %v", err)
	}

	s := &serviceProxy{managedKubeConfig: path}
	config, err := s.managedRESTConfig()
	if err != nil {
		t.Fatalf("unexpected managedRESTConfig error: %v", err)
	}
	if config.Host != "https://managed.example.com:6443" {
		t.Fatalf("unexpected managed host: %s", config.Host)
	}

	token, err := s.readImpersonateTokenFromManagedKubeconfig()
	if err != nil {
		t.Fatalf("unexpected token read error: %v", err)
	}
	if token != "managed-token" {
		t.Fatalf("expected managed-token, got %q", token)
	}
}

func TestParseManagedAPIServerURL(t *testing.T) {
	url, err := parseManagedAPIServerURL("https://managed.example.com:6443")
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if url.Host != "managed.example.com:6443" {
		t.Fatalf("unexpected host: %s", url.Host)
	}

	if _, err := parseManagedAPIServerURL("managed.example.com:6443"); err == nil {
		t.Fatal("expected error for URL without scheme")
	}
}

func TestOutboundTLSConfig_ReusesManagedKubeconfigTLS(t *testing.T) {
	managedTLS, err := rest.TLSConfigFor(&rest.Config{
		Host: "https://managed.example.com:6443",
		TLSClientConfig: rest.TLSClientConfig{
			ServerName: "managed.internal",
			Insecure:   false,
			CAData:     []byte(testCACert),
		},
	})
	if err != nil {
		t.Fatalf("unexpected rest.TLSConfigFor error: %v", err)
	}
	if managedTLS == nil {
		t.Fatal("expected non-nil managed TLS config")
	}

	rootCAs := x509.NewCertPool()
	s := &serviceProxy{
		rootCAs:                   rootCAs,
		managedAPIServerTLSConfig: managedTLS,
	}

	// When the target is the managed apiserver, reuse the managed kubeconfig TLS.
	managed := s.outboundTLSConfig(true)
	if managed.ServerName != "managed.internal" {
		t.Fatalf("expected ServerName to be reused, got %q", managed.ServerName)
	}
	if managed.RootCAs == nil {
		t.Fatal("expected managed RootCAs to be populated from managed kubeconfig")
	}
	if managed.MinVersion < tls.VersionTLS12 {
		t.Fatalf("expected MinVersion to be at least TLS 1.2, got %d", managed.MinVersion)
	}

	// Mutating the returned config must not leak back into the stored template.
	managed.ServerName = "mutated"
	if s.managedAPIServerTLSConfig.ServerName != "managed.internal" {
		t.Fatalf("outboundTLSConfig must clone the managed TLS config")
	}

	// When the target is not the managed apiserver, fall back to the local trust pool.
	fallback := s.outboundTLSConfig(false)
	if fallback.ServerName != "" {
		t.Fatalf("expected empty ServerName for non-managed target, got %q", fallback.ServerName)
	}
	if fallback.RootCAs != rootCAs {
		t.Fatal("expected fallback to use the in-cluster root CA pool")
	}
}

func TestOutboundTLSConfig_NoManagedKubeconfig(t *testing.T) {
	rootCAs := x509.NewCertPool()
	s := &serviceProxy{rootCAs: rootCAs}

	// Even when targetsManagedAPIServer is true, lack of managed kubeconfig
	// must fall back to the in-cluster trust pool rather than panic.
	cfg := s.outboundTLSConfig(true)
	if cfg.RootCAs != rootCAs {
		t.Fatal("expected fallback to use the in-cluster root CA pool when managed TLS is absent")
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("expected MinVersion TLS 1.2, got %d", cfg.MinVersion)
	}
}

// testCACert is a throwaway self-signed CA used only by TLS config tests.
const testCACert = `-----BEGIN CERTIFICATE-----
MIIBhTCCASugAwIBAgIQIRi6zePL6mKjOipn+dNuaTAKBggqhkjOPQQDAjASMRAw
DgYDVQQKEwdBY21lIENvMB4XDTE3MTAyMDE5NDMwNloXDTE4MTAyMDE5NDMwNlow
EjEQMA4GA1UEChMHQWNtZSBDbzBZMBMGByqGSM49AgEGCCqGSM49AwEHA0IABD0d
7VNhbWvZLWPuj/RtHFjvtJBEwOkhbN/BnnE8rnZR8+sbwnc/KhCk3FhnpHZnQz7B
5aETbbIgmuvewdjvSBSjYzBhMA4GA1UdDwEB/wQEAwICpDATBgNVHSUEDDAKBggr
BgEFBQcDATAPBgNVHRMBAf8EBTADAQH/MCkGA1UdEQQiMCCCDmxvY2FsaG9zdDo1
NDUzgg4xMjcuMC4wLjE6NTQ1MzAKBggqhkjOPQQDAgNIADBFAiEA2zpJEPQyz6/l
Wf86aX6PepsntZv2GYlA5UpabfT2EZICICpJ5h/iI+i341gBmLiAFQOyTDT+/wQc
6MF9+Yw1Yy0t
-----END CERTIFICATE-----
`

func TestServiceProxyRelayURLAndAuthorizationHeader(t *testing.T) {
	relayURLTemplate, err := buildServiceRelayURL("https://managed.example.com:6443", "addon-ns", "cluster-proxy-service-relay", 7444)
	if err != nil {
		t.Fatalf("unexpected buildServiceRelayURL error: %v", err)
	}
	s := &serviceProxy{
		relayURLTemplate:        relayURLTemplate,
		getImpersonateTokenFunc: func() (string, error) { return "managed-token", nil },
	}

	if s.relayURLTemplate.String() != "https://managed.example.com:6443/api/v1/namespaces/addon-ns/services/http:cluster-proxy-service-relay:7444/proxy" {
		t.Fatalf("unexpected relay URL %s", s.relayURLTemplate.String())
	}

	req, _ := http.NewRequest("GET", "https://example.com/ping", nil)
	req.Header.Set("Authorization", "Bearer original-token")
	req.Header.Set("Cluster-Proxy-Authorization", "Bearer spoofed-token")
	if err := s.prepareRelayRequest(req); err != nil {
		t.Fatalf("unexpected prepare relay request error: %v", err)
	}
	if req.Header.Get("Authorization") != "Bearer managed-token" {
		t.Fatalf("expected managed token authorization, got %q", req.Header.Get("Authorization"))
	}
	if req.Header.Get("Cluster-Proxy-Relay-Authorization") != "Bearer managed-token" {
		t.Fatalf("expected managed token relay authorization, got %q", req.Header.Get("Cluster-Proxy-Relay-Authorization"))
	}
	if req.Header.Get("Cluster-Proxy-Authorization") != "Bearer original-token" {
		t.Fatalf("expected original authorization in internal header, got %q", req.Header.Get("Cluster-Proxy-Authorization"))
	}
}

func TestProcessAuthentication_GetImpersonateTokenError(t *testing.T) {
	s := &serviceProxy{
		enableImpersonation: true,
		managedClusterAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return nil, false, nil
		}),
		hubAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return &authenticator.Response{
				User: &user.DefaultInfo{
					Name:   "system:serviceaccount:ns:my-sa",
					Groups: []string{"system:authenticated"},
				},
			}, true, nil
		}),
		getImpersonateTokenFunc: func() (string, error) {
			return "", fmt.Errorf("token file not found")
		},
	}

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer hub-token")

	err := s.processAuthentication(context.Background(), req)
	if err == nil {
		t.Fatal("expected error from getImpersonateTokenFunc")
	}
	if !strings.Contains(err.Error(), "failed to get impersonate token") {
		t.Fatalf("expected impersonate token error, got: %v", err)
	}
}

func TestProcessAuthentication_ManagedClusterFatalError(t *testing.T) {
	s := &serviceProxy{
		enableImpersonation: true,
		managedClusterAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return nil, false, fmt.Errorf("apiserver unreachable")
		}),
		hubAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			t.Fatal("hub authenticator should not be called for fatal managed cluster errors")
			return nil, false, nil
		}),
	}

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer some-token")

	err := s.processAuthentication(context.Background(), req)
	if err == nil {
		t.Fatal("expected fatal error when managed cluster auth has infrastructure failure")
	}
	if !strings.Contains(err.Error(), "apiserver unreachable") {
		t.Fatalf("expected original error preserved, got: %v", err)
	}
}

func TestProcessAuthentication_ManagedClusterRejection_FallsBackToHub(t *testing.T) {
	s := &serviceProxy{
		enableImpersonation: true,
		managedClusterAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return nil, false, nil // managed cluster rejected the token
		}),
		hubAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return &authenticator.Response{
				User: &user.DefaultInfo{
					Name:   "kube:admin",
					Groups: []string{"system:cluster-admins", "system:authenticated"},
				},
			}, true, nil
		}),
		getImpersonateTokenFunc: func() (string, error) {
			return "fake-sa-token", nil
		},
	}

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer hub-only-token")

	err := s.processAuthentication(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Header.Get("Impersonate-User") != "kube:admin" {
		t.Fatalf("expected impersonate user 'kube:admin', got '%s'", req.Header.Get("Impersonate-User"))
	}
	if req.Header.Get("Authorization") != "Bearer fake-sa-token" {
		t.Fatalf("expected authorization header to use impersonation token, got '%s'", req.Header.Get("Authorization"))
	}
}

func TestProcessAuthentication_HubAuthError(t *testing.T) {
	s := &serviceProxy{
		enableImpersonation: true,
		managedClusterAuthenticator: authenticator.TokenFunc(
			func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
				return nil, false, nil // not a managed cluster token
			}),
		hubAuthenticator: authenticator.TokenFunc(
			func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
				return nil, false, fmt.Errorf("hub apiserver timeout")
			}),
	}

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer some-token")

	err := s.processAuthentication(context.Background(), req)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "hub cluster auth error") {
		t.Fatalf("expected hub cluster auth error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "hub apiserver timeout") {
		t.Fatalf("expected original error message preserved, got: %v", err)
	}
}
