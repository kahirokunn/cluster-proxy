package managedapiserver

import (
	"context"
	"fmt"
	"net"
	"os"
	"testing"
	"time"
)

func TestTargetAddress(t *testing.T) {
	cases := []struct {
		name     string
		host     string
		expected string
	}{
		{
			name:     "https default port",
			host:     "https://managed.example.com",
			expected: "managed.example.com:443",
		},
		{
			name:     "https explicit port",
			host:     "https://managed.example.com:6443",
			expected: "managed.example.com:6443",
		},
		{
			name:     "http default port",
			host:     "http://managed.example.com",
			expected: "managed.example.com:80",
		},
		{
			name:     "ipv6 default port",
			host:     "https://[::1]",
			expected: "[::1]:443",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			actual, err := targetAddress(c.host)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if actual != c.expected {
				t.Fatalf("expected %q, got %q", c.expected, actual)
			}
		})
	}

	if _, err := targetAddress("ftp://managed.example.com"); err == nil {
		t.Fatal("expected unsupported scheme error")
	}
}

func TestProxyRelaysRawTCPBytes(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen upstream: %v", err)
	}
	defer upstream.Close()

	go func() {
		conn, err := upstream.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		buf := make([]byte, 32)
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		_, _ = conn.Write([]byte("relay:" + string(buf[:n])))
	}()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to allocate listen address: %v", err)
	}
	listenAddress := listener.Addr().String()
	_ = listener.Close()

	kubeconfigPath := t.TempDir() + "/kubeconfig"
	writeKubeconfig(t, kubeconfigPath, upstream.Addr().String())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- (&Proxy{
			ManagedKubeconfig:      kubeconfigPath,
			Listen:                 listenAddress,
			DialTimeout:            time.Second,
			HealthProbeBindAddress: "127.0.0.1:0",
		}).Run(ctx)
	}()

	conn, err := dialEventually(ctx, listenAddress)
	if err != nil {
		t.Fatalf("failed to dial proxy: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("tls-client-hello")); err != nil {
		t.Fatalf("failed to write to proxy: %v", err)
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("failed to read proxy response: %v", err)
	}
	if string(buf[:n]) != "relay:tls-client-hello" {
		t.Fatalf("unexpected proxy response %q", string(buf[:n]))
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil && err != context.Canceled {
			t.Fatalf("unexpected proxy error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for proxy shutdown")
	}
}

func TestProxyReloadsTargetWhenKubeconfigServerChanges(t *testing.T) {
	first := newEchoUpstream(t, "first")
	defer first.close()
	second := newEchoUpstream(t, "second")
	defer second.close()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to allocate listen address: %v", err)
	}
	listenAddress := listener.Addr().String()
	_ = listener.Close()

	kubeconfigPath := t.TempDir() + "/kubeconfig"
	writeKubeconfig(t, kubeconfigPath, first.addr())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	proxy := &Proxy{
		ManagedKubeconfig:      kubeconfigPath,
		Listen:                 listenAddress,
		DialTimeout:            time.Second,
		HealthProbeBindAddress: "127.0.0.1:0",
		ReloadInterval:         20 * time.Millisecond,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- proxy.Run(ctx) }()

	if got := exchange(ctx, t, listenAddress, "hello"); got != "first:hello" {
		t.Fatalf("expected initial relay to first upstream, got %q", got)
	}

	writeKubeconfig(t, kubeconfigPath, second.addr())

	deadline := time.Now().Add(2 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		got = exchange(ctx, t, listenAddress, "hello")
		if got == "second:hello" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got != "second:hello" {
		t.Fatalf("proxy did not pick up new managed kubeconfig server; last response %q", got)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil && err != context.Canceled {
			t.Fatalf("unexpected proxy error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for proxy shutdown")
	}
}

type echoUpstream struct {
	listener net.Listener
	done     chan struct{}
}

func newEchoUpstream(t *testing.T, prefix string) *echoUpstream {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen upstream: %v", err)
	}
	u := &echoUpstream{listener: l, done: make(chan struct{})}
	go func() {
		defer close(u.done)
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 64)
				n, err := c.Read(buf)
				if err != nil {
					return
				}
				_, _ = c.Write([]byte(prefix + ":" + string(buf[:n])))
			}(conn)
		}
	}()
	return u
}

func (u *echoUpstream) addr() string { return u.listener.Addr().String() }

func (u *echoUpstream) close() {
	_ = u.listener.Close()
	<-u.done
}

func writeKubeconfig(t *testing.T, path, serverHostPort string) {
	t.Helper()
	contents := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: managed
  cluster:
    server: https://%s
contexts:
- name: managed
  context:
    cluster: managed
    user: cluster-proxy
current-context: managed
users:
- name: cluster-proxy
  user:
    token: token
`, serverHostPort)
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatalf("failed to write kubeconfig: %v", err)
	}
}

func exchange(ctx context.Context, t *testing.T, address, payload string) string {
	t.Helper()
	conn, err := dialEventually(ctx, address)
	if err != nil {
		t.Fatalf("failed to dial proxy: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("failed to write to proxy: %v", err)
	}
	buf := make([]byte, 128)
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		return ""
	}
	return string(buf[:n])
}

func dialEventually(ctx context.Context, address string) (net.Conn, error) {
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := (&net.Dialer{Timeout: 50 * time.Millisecond}).DialContext(ctx, "tcp", address)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		time.Sleep(10 * time.Millisecond)
	}
	return nil, lastErr
}
