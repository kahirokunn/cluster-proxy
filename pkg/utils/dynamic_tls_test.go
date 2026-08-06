package utils

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	sdktls "open-cluster-management.io/sdk-go/pkg/tls"
)

func TestDynamicTLSConfigReloadsWithoutRestart(t *testing.T) {
	const namespace = "test"
	client := fake.NewClientset()
	changed := make(chan struct{}, 1)
	dynamic, err := StartDynamicTLSConfigMapWatcher(t.Context(), client, namespace, func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	baseCallbackCalled := false
	base := &tls.Config{Certificates: []tls.Certificate{{}}}
	base.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		baseCallbackCalled = true
		return base, nil
	}
	serverConfig := dynamic.ServerConfig(base)
	if serverConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("initial MinVersion = %d, want TLS 1.2", serverConfig.MinVersion)
	}

	_, err = client.CoreV1().ConfigMaps(namespace).Create(t.Context(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: sdktls.ConfigMapName, Namespace: namespace},
		Data: map[string]string{
			sdktls.ConfigMapKeyMinVersion: "VersionTLS13",
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for TLS profile reload")
	}

	handshakeConfig, err := serverConfig.GetConfigForClient(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !baseCallbackCalled {
		t.Fatal("dynamic server config did not compose the base GetConfigForClient callback")
	}
	if handshakeConfig.MinVersion != tls.VersionTLS13 || handshakeConfig.MaxVersion != tls.VersionTLS13 {
		t.Fatalf("reloaded versions = min %d max %d, want TLS 1.3 only", handshakeConfig.MinVersion, handshakeConfig.MaxVersion)
	}
	if len(handshakeConfig.Certificates) != 1 {
		t.Fatal("dynamic server config lost the base certificate")
	}
	if base.MinVersion != 0 || base.MaxVersion != 0 {
		t.Fatal("dynamic server config mutated its base config")
	}
}

func TestDynamicTLSConfigKeepsLastKnownGoodProfile(t *testing.T) {
	const namespace = "test"
	client := fake.NewClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: sdktls.ConfigMapName, Namespace: namespace},
		Data: map[string]string{
			sdktls.ConfigMapKeyMinVersion: "VersionTLS13",
		},
	})
	changed := make(chan struct{}, 1)
	dynamic, err := StartDynamicTLSConfigMapWatcher(t.Context(), client, namespace, func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	var reloadGets atomic.Int32
	client.PrependReactor("get", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		reloadGets.Add(1)
		return false, nil, nil
	})

	configMap, err := client.CoreV1().ConfigMaps(namespace).Get(t.Context(), sdktls.ConfigMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	baselineGets := reloadGets.Load()
	configMap.Data[sdktls.ConfigMapKeyMinVersion] = "not-a-version"
	configMap, err = client.CoreV1().ConfigMaps(namespace).Update(t.Context(), configMap, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	eventually(t, 5*time.Second, func() bool { return reloadGets.Load() == baselineGets+1 }, "invalid TLS profile was not observed")
	select {
	case <-changed:
		t.Fatal("invalid TLS profile was published")
	default:
	}
	time.Sleep(1500 * time.Millisecond)
	if got := reloadGets.Load(); got != baselineGets+1 {
		t.Fatalf("invalid unchanged profile caused %d reload GETs, want exactly one", got-baselineGets)
	}
	if got := dynamic.Current().MinVersion; got != tls.VersionTLS13 {
		t.Fatalf("MinVersion = %d after invalid update, want last known-good TLS 1.3", got)
	}

	configMap.Data[sdktls.ConfigMapKeyMinVersion] = "VersionTLS12"
	if _, err := client.CoreV1().ConfigMaps(namespace).Update(t.Context(), configMap, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for valid TLS profile after invalid update")
	}
	if got := dynamic.Current().MinVersion; got != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %d after corrected update, want TLS 1.2", got)
	}
}

func TestDynamicTLSConfigKeepsLastKnownGoodProfileWhenServingCertificateIsIncompatible(t *testing.T) {
	const namespace = "test"
	client := fake.NewClientset()
	var rejectTLS13 atomic.Bool
	rejectTLS13.Store(true)
	changed := make(chan struct{}, 1)
	dynamic, err := StartDynamicTLSConfigMapWatcherWithValidator(
		t.Context(),
		client,
		namespace,
		func(profile *sdktls.TLSConfig) error {
			if rejectTLS13.Load() && profile.MinVersion == tls.VersionTLS13 {
				return errors.New("serving certificate is incompatible")
			}
			return nil
		},
		func() {
			select {
			case changed <- struct{}{}:
			default:
			}
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	configMap, err := client.CoreV1().ConfigMaps(namespace).Create(t.Context(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: sdktls.ConfigMapName, Namespace: namespace},
		Data: map[string]string{
			sdktls.ConfigMapKeyMinVersion: "VersionTLS13",
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	select {
	case <-changed:
		t.Fatal("certificate-incompatible TLS profile was published")
	default:
	}
	if got := dynamic.Current().MinVersion; got != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %d after incompatible update, want last known-good TLS 1.2", got)
	}

	rejectTLS13.Store(false)
	configMap.Data[sdktls.ConfigMapKeyCipherSuites] = "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"
	if _, err := client.CoreV1().ConfigMaps(namespace).Update(t.Context(), configMap, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for corrected serving TLS profile")
	}
	if got := dynamic.Current().MinVersion; got != tls.VersionTLS13 {
		t.Fatalf("MinVersion = %d after corrected update, want TLS 1.3", got)
	}
}

func TestServingTLSProfileValidatorUsesTheRealCertificate(t *testing.T) {
	certificate := newRSAServingCertificate(t)
	validate := ServingTLSProfileValidator(&tls.Config{Certificates: []tls.Certificate{certificate}})

	if err := validate(&sdktls.TLSConfig{
		MinVersion:   tls.VersionTLS12,
		CipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
	}); err != nil {
		t.Fatalf("RSA-compatible TLS 1.2 profile was rejected: %v", err)
	}
	if err := validate(&sdktls.TLSConfig{
		MinVersion:   tls.VersionTLS12,
		CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
	}); err == nil {
		t.Fatal("ECDSA-only TLS 1.2 profile unexpectedly passed with an RSA certificate")
	}
	if err := validate(&sdktls.TLSConfig{
		MinVersion:   tls.VersionTLS13,
		CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
	}); err != nil {
		t.Fatalf("TLS 1.3 profile incorrectly applied its TLS 1.2 cipher list: %v", err)
	}
}

func TestDynamicTLSConfigChangesOnlyFreshNetworkHandshakes(t *testing.T) {
	seed := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	certificate := seed.TLS.Certificates[0]
	seed.Close()

	dynamic := NewDynamicTLSConfig(&sdktls.TLSConfig{MinVersion: tls.VersionTLS13})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{
		TLSConfig: dynamic.ServerConfig(&tls.Config{
			Certificates: []tls.Certificate{certificate},
		}),
		Handler: http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			response.Header().Set("Content-Length", "2")
			_, _ = response.Write([]byte("ok"))
		}),
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.ServeTLS(listener, "", "")
	}()
	t.Cleanup(func() {
		_ = server.Close()
		if err := <-serveDone; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("TLS test server exited: %v", err)
		}
	})

	dial := func(minVersion, maxVersion uint16, cipherSuites []uint16) (*tls.Conn, error) {
		return tls.Dial("tcp", listener.Addr().String(), &tls.Config{ //nolint:gosec -- isolated test server certificate.
			InsecureSkipVerify: true,
			MinVersion:         minVersion,
			MaxVersion:         maxVersion,
			CipherSuites:       cipherSuites,
		})
	}
	assertHTTP2 := func() {
		t.Helper()
		transport := &http.Transport{
			ForceAttemptHTTP2: true,
			TLSClientConfig: &tls.Config{ //nolint:gosec -- isolated test server certificate.
				InsecureSkipVerify: true,
				MinVersion:         tls.VersionTLS13,
			},
		}
		defer transport.CloseIdleConnections()
		response, err := (&http.Client{Transport: transport, Timeout: 5 * time.Second}).Get("https://" + listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.ProtoMajor != 2 {
			t.Fatalf("dynamic TLS server negotiated %q, want HTTP/2", response.Proto)
		}
	}
	request := func(connection *tls.Conn) error {
		if _, err := fmt.Fprintf(connection, "GET / HTTP/1.1\r\nHost: test\r\n\r\n"); err != nil {
			return err
		}
		response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodGet})
		if err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			_ = response.Body.Close()
			return err
		}
		return response.Body.Close()
	}

	if connection, err := dial(tls.VersionTLS12, tls.VersionTLS12, nil); err == nil {
		_ = connection.Close()
		t.Fatal("TLS 1.2 handshake unexpectedly succeeded against TLS 1.3 profile")
	}
	tls13Connection, err := dial(tls.VersionTLS13, tls.VersionTLS13, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tls13Connection.Close()
	if err := request(tls13Connection); err != nil {
		t.Fatal(err)
	}
	assertHTTP2()

	tls12Cipher := uint16(tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256)
	dynamic.store(&sdktls.TLSConfig{MinVersion: tls.VersionTLS12, CipherSuites: []uint16{tls12Cipher}})
	if err := request(tls13Connection); err != nil {
		t.Fatalf("established TLS 1.3 connection was interrupted by profile update: %v", err)
	}
	tls12Connection, err := dial(tls.VersionTLS12, tls.VersionTLS12, []uint16{tls12Cipher})
	if err != nil {
		t.Fatalf("fresh TLS 1.2 handshake did not adopt updated profile: %v", err)
	}
	defer tls12Connection.Close()
	if err := request(tls12Connection); err != nil {
		t.Fatal(err)
	}
	assertHTTP2()

	dynamic.store(&sdktls.TLSConfig{MinVersion: tls.VersionTLS13})
	if err := request(tls12Connection); err != nil {
		t.Fatalf("established TLS 1.2 connection was interrupted by reverse profile update: %v", err)
	}
	if connection, err := dial(tls.VersionTLS12, tls.VersionTLS12, []uint16{tls12Cipher}); err == nil {
		_ = connection.Close()
		t.Fatal("fresh TLS 1.2 handshake ignored restored TLS 1.3 profile")
	}
	freshTLS13, err := dial(tls.VersionTLS13, tls.VersionTLS13, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = freshTLS13.Close()
	assertHTTP2()
}

func TestDynamicTLSConfigRetriesARecognizedUpdateAfterTransientGetFailure(t *testing.T) {
	const namespace = "test"
	client := fake.NewClientset()
	var failNextGet atomic.Bool
	client.PrependReactor("get", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		if failNextGet.Swap(false) {
			return true, nil, errors.New("transient API failure")
		}
		return false, nil, nil
	})
	changed := make(chan struct{}, 1)
	dynamic, err := StartDynamicTLSConfigMapWatcher(t.Context(), client, namespace, func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	failNextGet.Store(true)
	_, err = client.CoreV1().ConfigMaps(namespace).Create(t.Context(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: sdktls.ConfigMapName, Namespace: namespace},
		Data: map[string]string{
			sdktls.ConfigMapKeyMinVersion: "VersionTLS13",
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("TLS update was lost after a transient GET failure")
	}
	if got := dynamic.Current().MinVersion; got != tls.VersionTLS13 {
		t.Fatalf("MinVersion = %d after retry, want TLS 1.3", got)
	}
}

func TestDynamicTLSConfigCancellationStopsAPIErrorRetries(t *testing.T) {
	const namespace = "test"
	client := fake.NewClientset()
	ctx, cancel := context.WithCancel(t.Context())
	_, err := StartDynamicTLSConfigMapWatcher(ctx, client, namespace, nil)
	if err != nil {
		t.Fatal(err)
	}
	var reloadGets atomic.Int32
	client.PrependReactor("get", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		reloadGets.Add(1)
		return true, nil, errors.New("API remains unavailable")
	})
	_, err = client.CoreV1().ConfigMaps(namespace).Create(t.Context(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: sdktls.ConfigMapName, Namespace: namespace},
		Data: map[string]string{
			sdktls.ConfigMapKeyMinVersion: "VersionTLS13",
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, func() bool { return reloadGets.Load() > 0 }, "TLS reload retry did not start")
	cancel()
	time.Sleep(100 * time.Millisecond)
	stoppedAt := reloadGets.Load()
	time.Sleep(500 * time.Millisecond)
	if got := reloadGets.Load(); got != stoppedAt {
		t.Fatalf("TLS reload performed %d API calls after cancellation", got-stoppedAt)
	}
}

func eventually(t *testing.T, timeout time.Duration, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(message)
}

func newRSAServingCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "dynamic-tls.test"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  privateKey,
	}
}
