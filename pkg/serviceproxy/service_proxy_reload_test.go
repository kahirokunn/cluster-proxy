package serviceproxy

import (
	"os"
	"path/filepath"
	"testing"
)

func writeManagedKubeconfig(t *testing.T, dir, server, caData, token string) string {
	t.Helper()
	contents := `apiVersion: v1
kind: Config
clusters:
- name: managed
  cluster:
    server: ` + server + `
    certificate-authority-data: ` + caData + `
users:
- name: cluster-proxy
  user:
    token: ` + token + `
contexts:
- name: managed
  context:
    cluster: managed
    user: cluster-proxy
current-context: managed
`
	path := filepath.Join(dir, "managed.kubeconfig")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

func TestManagedKubeconfigReloadChecker_IgnoresTokenRefresh(t *testing.T) {
	dir := t.TempDir()
	// base64("test-ca") so the kubeconfig parses; the exact bytes are irrelevant.
	const caData = "dGVzdC1jYQ=="
	path := writeManagedKubeconfig(t, dir, "https://managed.example:6443", caData, "token-old")

	checker, err := newManagedKubeconfigReloadChecker(path)
	if err != nil {
		t.Fatalf("newManagedKubeconfigReloadChecker: %v", err)
	}

	if err := checker(nil); err != nil {
		t.Fatalf("checker should be healthy before any change, got: %v", err)
	}

	// Routine token rotation must NOT mark the proxy unhealthy.
	writeManagedKubeconfig(t, dir, "https://managed.example:6443", caData, "token-new")
	if err := checker(nil); err != nil {
		t.Fatalf("token-only refresh should not trigger restart, got: %v", err)
	}
}

func TestManagedKubeconfigReloadChecker_DetectsEndpointChange(t *testing.T) {
	dir := t.TempDir()
	const caData = "dGVzdC1jYQ=="
	path := writeManagedKubeconfig(t, dir, "https://managed.example:6443", caData, "token-old")

	checker, err := newManagedKubeconfigReloadChecker(path)
	if err != nil {
		t.Fatalf("newManagedKubeconfigReloadChecker: %v", err)
	}

	writeManagedKubeconfig(t, dir, "https://new-endpoint.example:6443", caData, "token-old")
	if err := checker(nil); err == nil {
		t.Fatal("endpoint change should trigger restart, got healthy")
	}
}

func TestManagedKubeconfigReloadChecker_DetectsTLSChange(t *testing.T) {
	dir := t.TempDir()
	path := writeManagedKubeconfig(t, dir, "https://managed.example:6443", "dGVzdC1jYQ==", "token-old")

	checker, err := newManagedKubeconfigReloadChecker(path)
	if err != nil {
		t.Fatalf("newManagedKubeconfigReloadChecker: %v", err)
	}

	// Rotated CA bundle (base64("new-ca")) must trigger a restart.
	writeManagedKubeconfig(t, dir, "https://managed.example:6443", "bmV3LWNh", "token-old")
	if err := checker(nil); err == nil {
		t.Fatal("CA/TLS change should trigger restart, got healthy")
	}
}
