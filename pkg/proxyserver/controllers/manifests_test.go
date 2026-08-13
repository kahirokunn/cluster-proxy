package controllers

import (
	"crypto/tls"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	sdktls "open-cluster-management.io/sdk-go/pkg/tls"
)

func newTestConfig(replicas int32, additionalArgs ...string) *proxyv1alpha1.ManagedProxyConfiguration {
	return &proxyv1alpha1.ManagedProxyConfiguration{
		Spec: proxyv1alpha1.ManagedProxyConfigurationSpec{
			ProxyServer: proxyv1alpha1.ManagedProxyConfigurationProxyServer{
				Replicas:       replicas,
				AdditionalArgs: additionalArgs,
			},
		},
	}
}

var baseArgs = []string{
	"--server-count=3",
	"--proxy-strategies=destHost",
	"--health-port=8092",
	"--server-ca-cert=/etc/server-ca-pki/ca.crt",
	"--server-cert=/etc/server-pki/tls.crt",
	"--server-key=/etc/server-pki/tls.key",
	"--cluster-ca-cert=/etc/server-ca-pki/ca.crt",
	"--cluster-cert=/etc/agent-pki/tls.crt",
	"--cluster-key=/etc/agent-pki/tls.key",
}

func TestProxyServerArgs_NilTLSConfig(t *testing.T) {
	args := proxyServerArgs(newTestConfig(3), nil)
	assert.Equal(t, baseArgs, args)
}

func TestProxyServerArgs_EmptyCipherSuites(t *testing.T) {
	args := proxyServerArgs(newTestConfig(3), &sdktls.TLSConfig{})
	assert.Equal(t, baseArgs, args)
}

func TestProxyServerArgs_WithCipherSuites(t *testing.T) {
	tlsConfig := &sdktls.TLSConfig{
		CipherSuites: []uint16{
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		},
	}
	args := proxyServerArgs(newTestConfig(3), tlsConfig)

	expected := append(append([]string{}, baseArgs...),
		"--cipher-suites="+sdktls.CipherSuitesToString(tlsConfig.CipherSuites),
	)
	assert.Equal(t, expected, args)
}

func TestProxyServerArgs_WithAdditionalArgs(t *testing.T) {
	config := newTestConfig(3, "--extra-flag=value")
	args := proxyServerArgs(config, nil)

	expected := append(append([]string{}, baseArgs...), "--extra-flag=value")
	assert.Equal(t, expected, args)
}

func TestProxyServerArgs_WithAdditionalArgsAndCipherSuites(t *testing.T) {
	config := newTestConfig(3, "--extra-flag=value")
	tlsConfig := &sdktls.TLSConfig{
		CipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
	}
	args := proxyServerArgs(config, tlsConfig)

	expected := append(append([]string{}, baseArgs...),
		"--extra-flag=value",
		"--cipher-suites="+sdktls.CipherSuitesToString(tlsConfig.CipherSuites),
	)
	assert.Equal(t, expected, args)
}

func TestNewProxyServerDeployment_SetsPodSecurityContext(t *testing.T) {
	config := newTestConfig(3)
	config.Name = "cluster-proxy"
	config.Spec.ProxyServer.Namespace = "test"
	config.Spec.ProxyServer.Image = "quay.io/open-cluster-management/cluster-proxy:test"

	deploy, err := newProxyServerDeployment(config, "IfNotPresent", nil)
	if err != nil {
		t.Fatalf("unexpected deployment error: %v", err)
	}

	expected := &corev1.PodSecurityContext{
		RunAsNonRoot: ptr.To(true),
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
	assert.Equal(t, expected, deploy.Spec.Template.Spec.SecurityContext)
}

func TestNewProxyServerDeployment_SetsHTTPProbes(t *testing.T) {
	config := newTestConfig(3)
	config.Name = "cluster-proxy"
	config.Spec.ProxyServer.Namespace = "test"
	config.Spec.ProxyServer.Image = "quay.io/open-cluster-management/cluster-proxy:test"

	deploy, err := newProxyServerDeployment(config, "IfNotPresent", nil)
	if err != nil {
		t.Fatalf("unexpected deployment error: %v", err)
	}
	if len(deploy.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("unexpected container count: got %d, want 1", len(deploy.Spec.Template.Spec.Containers))
	}
	container := deploy.Spec.Template.Spec.Containers[0]

	assertHTTPProbe := func(name string, probe *corev1.Probe) {
		t.Helper()
		if probe == nil {
			t.Fatalf("%s probe is nil", name)
		}
		if probe.HTTPGet == nil {
			t.Fatalf("%s HTTP GET is nil", name)
		}
		assert.Equal(t, "/healthz", probe.HTTPGet.Path)
		assert.Equal(t, defaultProxyServerHealthPort, probe.HTTPGet.Port.IntVal)
		assert.Equal(t, corev1.URISchemeHTTP, probe.HTTPGet.Scheme)
		assert.Nil(t, probe.TCPSocket)
		assert.Nil(t, probe.Exec)
	}

	assertHTTPProbe("liveness", container.LivenessProbe)
	assert.Equal(t, int32(10), container.LivenessProbe.InitialDelaySeconds)
	assert.Equal(t, int32(10), container.LivenessProbe.PeriodSeconds)
	assert.Equal(t, int32(2), container.LivenessProbe.TimeoutSeconds)
	assert.Equal(t, int32(3), container.LivenessProbe.FailureThreshold)
	assertHTTPProbe("readiness", container.ReadinessProbe)
	assert.Equal(t, int32(5), container.ReadinessProbe.InitialDelaySeconds)
	assert.Equal(t, int32(2), container.ReadinessProbe.PeriodSeconds)
	assert.Equal(t, int32(1), container.ReadinessProbe.TimeoutSeconds)
	assert.Equal(t, int32(1), container.ReadinessProbe.FailureThreshold)
	assert.Nil(t, container.StartupProbe)
}

func TestNewProxyServerDeployment_UsesTypedHealthPort(t *testing.T) {
	config := newTestConfig(3)
	config.Spec.Deploy = &proxyv1alpha1.ManagedProxyConfigurationDeploy{
		Ports: proxyv1alpha1.ManagedProxyConfigurationDeployPorts{HealthServer: 18092},
	}

	deploy, err := newProxyServerDeployment(config, "IfNotPresent", nil)
	if err != nil {
		t.Fatalf("unexpected deployment error: %v", err)
	}
	container := deploy.Spec.Template.Spec.Containers[0]
	assert.Equal(t, int32(18092), container.LivenessProbe.HTTPGet.Port.IntVal)
	assert.Equal(t, int32(18092), container.ReadinessProbe.HTTPGet.Port.IntVal)
	assert.Contains(t, container.Args, "--health-port=18092")
	assert.NotContains(t, container.Args, "--health-bind-address=0.0.0.0")
}

func TestNewProxyServerDeployment_RejectsReservedHealthArgs(t *testing.T) {
	for _, arg := range []string{
		"--health-port=18092",
		"--health-port",
		"--health-bind-address=127.0.0.1",
		"--health-bind-address",
	} {
		t.Run(arg, func(t *testing.T) {
			_, err := newProxyServerDeployment(newTestConfig(3, arg), "IfNotPresent", nil)
			if err == nil {
				t.Fatal("expected reserved health argument to be rejected")
			}
			assert.Contains(t, err.Error(), "reserved flag")
		})
	}
}

func TestNewProxyServerDeployment_RejectsOutOfRangeHealthPort(t *testing.T) {
	for _, port := range []int32{-1, 0, 1023, 49152} {
		t.Run(strconv.Itoa(int(port)), func(t *testing.T) {
			config := newTestConfig(3)
			config.Spec.Deploy = &proxyv1alpha1.ManagedProxyConfigurationDeploy{
				Ports: proxyv1alpha1.ManagedProxyConfigurationDeployPorts{HealthServer: port},
			}

			_, err := newProxyServerDeployment(config, "IfNotPresent", nil)
			if err == nil {
				t.Fatal("expected out-of-range health port to be rejected")
			}
			assert.Contains(t, err.Error(), "must be between 1024 and 49151")
		})
	}
}

func TestTLSConfigHash_Nil(t *testing.T) {
	assert.Equal(t, "", tlsConfigHash(nil))
}

func TestTLSConfigHash_Deterministic(t *testing.T) {
	tlsConfig := &sdktls.TLSConfig{
		CipherSuites: []uint16{
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		},
		MinVersion: tls.VersionTLS12,
	}
	hash1 := tlsConfigHash(tlsConfig)
	hash2 := tlsConfigHash(tlsConfig)
	assert.Equal(t, hash1, hash2)
	assert.Len(t, hash1, 16)
}

func TestTLSConfigHash_DiffersOnChange(t *testing.T) {
	config1 := &sdktls.TLSConfig{
		CipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
	}
	config2 := &sdktls.TLSConfig{
		CipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384},
	}
	assert.NotEqual(t, tlsConfigHash(config1), tlsConfigHash(config2))
}

func TestTLSConfigHash_EmptyConfig(t *testing.T) {
	hash := tlsConfigHash(&sdktls.TLSConfig{})
	assert.NotEmpty(t, hash)
	assert.Len(t, hash, 16)
}

func TestProxyServerArgs_WithMinVersion(t *testing.T) {
	tests := []struct {
		name       string
		minVersion uint16
		expected   string
	}{
		{
			name:       "TLS12",
			minVersion: tls.VersionTLS12,
			expected:   "--tls-min-version=VersionTLS12",
		},
		{
			name:       "TLS13",
			minVersion: tls.VersionTLS13,
			expected:   "--tls-min-version=VersionTLS13",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tlsConfig := &sdktls.TLSConfig{
				MinVersion: tt.minVersion,
			}
			args := proxyServerArgs(newTestConfig(3), tlsConfig)

			expected := append(append([]string{}, baseArgs...), tt.expected)
			assert.Equal(t, expected, args)
		})
	}
}

func TestProxyServerArgs_WithMinVersionAndCipherSuites(t *testing.T) {
	tlsConfig := &sdktls.TLSConfig{
		CipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
		MinVersion:   tls.VersionTLS13,
	}
	args := proxyServerArgs(newTestConfig(3), tlsConfig)

	expected := append(append([]string{}, baseArgs...),
		"--cipher-suites="+sdktls.CipherSuitesToString(tlsConfig.CipherSuites),
		"--tls-min-version="+sdktls.VersionToString(tlsConfig.MinVersion),
	)
	assert.Equal(t, expected, args)
}
