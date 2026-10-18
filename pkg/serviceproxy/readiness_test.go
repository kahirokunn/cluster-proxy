package serviceproxy

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestTLSHealthChecker(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := server.Listener.Addr().String()
	checker := newTLSHealthChecker(address, tlsConfigForServer(server), time.Second)

	for range 3 {
		if err := checker(httptest.NewRequest(http.MethodGet, "/readyz", nil)); err != nil {
			t.Fatalf("expected a healthy TLS server: %v", err)
		}
	}

	var wg sync.WaitGroup
	errors := make(chan error, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errors <- checker(httptest.NewRequest(http.MethodGet, "/readyz", nil))
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Errorf("expected concurrent checks to succeed: %v", err)
		}
	}

	server.Close()
	if err := checker(httptest.NewRequest(http.MethodGet, "/readyz", nil)); err == nil {
		t.Fatal("expected a stopped TLS server to be unhealthy")
	}

	server = newTLSServerAt(t, address)
	t.Cleanup(server.Close)
	if err := checker(httptest.NewRequest(http.MethodGet, "/readyz", nil)); err != nil {
		t.Fatalf("expected a restarted TLS server to be healthy: %v", err)
	}
}

func TestTLSHealthCheckerRejectsInvalidServers(t *testing.T) {
	t.Run("untrusted certificate", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		defer server.Close()

		checker := newTLSHealthChecker(server.Listener.Addr().String(), &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    x509.NewCertPool(),
			ServerName: "127.0.0.1",
		}, time.Second)
		if err := checker(httptest.NewRequest(http.MethodGet, "/readyz", nil)); err == nil {
			t.Fatal("expected an untrusted TLS server to be unhealthy")
		}
	})

	t.Run("non-TLS listener", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		defer server.Close()

		checker := newTLSHealthChecker(server.Listener.Addr().String(), &tls.Config{
			MinVersion: tls.VersionTLS12,
		}, time.Second)
		if err := checker(httptest.NewRequest(http.MethodGet, "/readyz", nil)); err == nil {
			t.Fatal("expected a non-TLS server to be unhealthy")
		}
	})

	t.Run("handshake timeout", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()

		accepted := make(chan net.Conn, 1)
		go func() {
			conn, err := listener.Accept()
			if err == nil {
				accepted <- conn
			}
			close(accepted)
		}()

		checker := newTLSHealthChecker(listener.Addr().String(), &tls.Config{
			MinVersion: tls.VersionTLS12,
		}, 20*time.Millisecond)
		if err := checker(httptest.NewRequest(http.MethodGet, "/readyz", nil)); err == nil {
			t.Fatal("expected a stalled TLS handshake to be unhealthy")
		}

		if conn := <-accepted; conn != nil {
			conn.Close()
		}
	})
}

func tlsConfigForServer(server *httptest.Server) *tls.Config {
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
		ServerName: "127.0.0.1",
	}
}

func newTLSServerAt(t *testing.T, address string) *httptest.Server {
	t.Helper()

	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Listener = listener
	server.StartTLS()
	return server
}
