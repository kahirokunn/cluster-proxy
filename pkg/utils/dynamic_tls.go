package utils

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	sdktls "open-cluster-management.io/sdk-go/pkg/tls"
)

const tlsConfigReloadTimeout = 10 * time.Second

const (
	tlsConfigReloadInitialRetry = 250 * time.Millisecond
	tlsConfigReloadMaximumRetry = 30 * time.Second
)

// DynamicTLSConfig keeps the last valid OCM TLS profile and applies it to new
// TLS handshakes. Existing connections are deliberately left alone so a
// profile update does not interrupt in-flight proxy traffic.
type DynamicTLSConfig struct {
	current atomic.Pointer[sdktls.TLSConfig]
}

// TLSProfileValidator rejects a syntactically valid profile that a particular
// serving certificate or endpoint cannot actually use.
type TLSProfileValidator func(*sdktls.TLSConfig) error

// StartDynamicTLSConfigMapWatcher loads the initial OCM TLS profile and watches
// it for changes. Invalid updates are logged and ignored, preserving the last
// known-good profile. onChange runs only after a valid, materially different
// profile has been installed.
func StartDynamicTLSConfigMapWatcher(
	ctx context.Context,
	client kubernetes.Interface,
	namespace string,
	onChange func(),
) (*DynamicTLSConfig, error) {
	return StartDynamicTLSConfigMapWatcherWithValidator(ctx, client, namespace, nil, onChange)
}

// StartDynamicTLSConfigMapWatcherWithValidator also verifies each profile
// against caller-specific serving material before atomically installing it.
func StartDynamicTLSConfigMapWatcherWithValidator(
	ctx context.Context,
	client kubernetes.Interface,
	namespace string,
	validate TLSProfileValidator,
	onChange func(),
) (*DynamicTLSConfig, error) {
	dynamic := NewDynamicTLSConfig(sdktls.GetDefaultTLSConfig())
	reloadSignals := make(chan struct{}, 1)
	signalReload := func() {
		select {
		case reloadSignals <- struct{}{}:
		default:
		}
	}

	initial, err := sdktls.StartTLSConfigMapWatcher(ctx, client, namespace, signalReload)
	if err != nil {
		return nil, fmt.Errorf("start TLS ConfigMap watcher: %w", err)
	}
	if validate != nil {
		if err := validate(initial); err != nil {
			return nil, fmt.Errorf("validate initial TLS profile: %w", err)
		}
	}
	dynamic.store(initial)
	go dynamic.runReloadWorker(ctx, client, namespace, reloadSignals, validate, onChange)
	return dynamic, nil
}

// NewDynamicTLSConfig creates an in-memory dynamic profile. Most production
// callers should use StartDynamicTLSConfigMapWatcher; this constructor is also
// useful when a caller receives profile updates through another durable source.
func NewDynamicTLSConfig(initial *sdktls.TLSConfig) *DynamicTLSConfig {
	dynamic := &DynamicTLSConfig{}
	dynamic.store(initial)
	return dynamic
}

// Current returns an immutable snapshot of the active TLS profile.
func (d *DynamicTLSConfig) Current() *sdktls.TLSConfig {
	return cloneTLSProfile(d.current.Load())
}

// ServerConfig returns a TLS config that snapshots the current profile for
// each new client handshake. base is cloned and is never mutated.
func (d *DynamicTLSConfig) ServerConfig(base *tls.Config) *tls.Config {
	config := d.apply(base)
	config.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		selected := base
		if base != nil && base.GetConfigForClient != nil {
			var err error
			selected, err = base.GetConfigForClient(hello)
			if err != nil {
				return nil, err
			}
			if selected == nil {
				selected = base
			}
		}
		handshakeConfig := d.apply(selected)
		handshakeConfig.GetConfigForClient = nil
		// http.Server.ServeTLS installs its final ALPN policy on the config
		// returned by this method before accepting connections. Preserve that
		// finalized policy while still rebuilding version/cipher fields from the
		// pristine base, which is necessary for reverse profile transitions.
		handshakeConfig.NextProtos = append([]string(nil), config.NextProtos...)
		return handshakeConfig, nil
	}
	return config
}

// ClientConfig returns a new client TLS config using the current profile.
func (d *DynamicTLSConfig) ClientConfig(base *tls.Config) *tls.Config {
	return d.apply(base)
}

// ServingTLSProfileValidator proves that the configured minimum protocol and
// TLS 1.2 cipher list can complete a handshake with base's real serving
// certificate. TLS 1.3 suites are selected by Go and are intentionally not
// configurable.
func ServingTLSProfileValidator(base *tls.Config) TLSProfileValidator {
	if base == nil {
		return func(*sdktls.TLSConfig) error {
			return errors.New("serving TLS base config is nil")
		}
	}
	pristine := base.Clone()
	return func(profile *sdktls.TLSConfig) error {
		serverConfig := pristine.Clone()
		sdktls.ConfigToFunc(profile)(serverConfig)

		version := serverConfig.MinVersion
		if version == 0 {
			version = tls.VersionTLS12
		}
		serverConfig.MaxVersion = version
		clientConfig := &tls.Config{
			InsecureSkipVerify: true, // The handshake validates compatibility, not trust.
			MinVersion:         version,
			MaxVersion:         version,
			CipherSuites:       append([]uint16(nil), serverConfig.CipherSuites...),
		}

		serverSide, clientSide := net.Pipe()
		defer serverSide.Close()
		defer clientSide.Close()
		deadline := time.Now().Add(tlsConfigReloadTimeout)
		_ = serverSide.SetDeadline(deadline)
		_ = clientSide.SetDeadline(deadline)

		serverResult := make(chan error, 1)
		go func() {
			serverResult <- tls.Server(serverSide, serverConfig).Handshake()
		}()
		clientErr := tls.Client(clientSide, clientConfig).Handshake()
		serverErr := <-serverResult
		if err := errors.Join(clientErr, serverErr); err != nil {
			return fmt.Errorf("serving certificate cannot negotiate TLS %s with the configured cipher suites: %w",
				sdktls.VersionToString(version), err)
		}
		return nil
	}
}

func (d *DynamicTLSConfig) apply(base *tls.Config) *tls.Config {
	var config *tls.Config
	if base == nil {
		config = &tls.Config{}
	} else {
		config = base.Clone()
	}
	sdktls.ConfigToFunc(d.Current())(config)
	return config
}

func (d *DynamicTLSConfig) store(cfg *sdktls.TLSConfig) bool {
	if cfg == nil {
		cfg = sdktls.GetDefaultTLSConfig()
	}
	next := cloneTLSProfile(cfg)
	previous := d.current.Swap(next)
	return previous == nil || previous.MinVersion != next.MinVersion ||
		!slices.Equal(previous.CipherSuites, next.CipherSuites)
}

func (d *DynamicTLSConfig) runReloadWorker(
	ctx context.Context,
	client kubernetes.Interface,
	namespace string,
	reloadSignals <-chan struct{},
	validate TLSProfileValidator,
	onChange func(),
) {
	retryDelay := tlsConfigReloadInitialRetry
	for {
		select {
		case <-ctx.Done():
			return
		case <-reloadSignals:
		}

		for {
			reloadCtx, cancel := context.WithTimeout(ctx, tlsConfigReloadTimeout)
			cfg, validationErr, err := loadTLSProfile(reloadCtx, client, namespace)
			cancel()
			if err == nil && validate != nil {
				if validateErr := validate(cfg); validateErr != nil {
					validationErr = true
					err = validateErr
				}
			}
			if err == nil {
				if d.store(cfg) {
					klog.Infof("TLS config reloaded without restarting: minVersion=%s, ciphersuites=%s",
						sdktls.VersionToString(cfg.MinVersion), sdktls.CipherSuitesToString(cfg.CipherSuites))
					if onChange != nil {
						onChange()
					}
				}
				retryDelay = tlsConfigReloadInitialRetry
				break
			}
			if validationErr {
				klog.Errorf("TLS ConfigMap is invalid; continuing with the last known-good profile until the object changes: %v", err)
				retryDelay = tlsConfigReloadInitialRetry
				break
			}

			// The SDK watcher has already acknowledged this object hash. Retry
			// durably so a transient API error cannot lose a valid profile merely
			// because no further watch event occurs.
			klog.Errorf("TLS ConfigMap reload failed; continuing with the last known-good profile and retrying: %v", err)
			timer := time.NewTimer(jitterDuration(retryDelay))
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return
			case <-reloadSignals:
				if !timer.Stop() {
					<-timer.C
				}
				retryDelay = tlsConfigReloadInitialRetry
			case <-timer.C:
				retryDelay = min(retryDelay*2, tlsConfigReloadMaximumRetry)
			}
		}
	}
}

// loadTLSProfile separates API failures, which need durable retries after the
// informer has acknowledged an object hash, from invalid data, which must keep
// the LKG and wait for an operator correction.
func loadTLSProfile(
	ctx context.Context,
	client kubernetes.Interface,
	namespace string,
) (cfg *sdktls.TLSConfig, validationErr bool, err error) {
	configMap, err := client.CoreV1().ConfigMaps(namespace).Get(ctx, sdktls.ConfigMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return sdktls.GetDefaultTLSConfig(), false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return parseTLSProfile(configMap)
}

func parseTLSProfile(configMap *corev1.ConfigMap) (*sdktls.TLSConfig, bool, error) {
	if configMap == nil {
		return nil, true, fmt.Errorf("ConfigMap is nil")
	}
	minVersion, err := sdktls.ParseTLSVersion(configMap.Data[sdktls.ConfigMapKeyMinVersion])
	if err != nil {
		return nil, true, fmt.Errorf("invalid minTLSVersion in ConfigMap: %w", err)
	}
	profile := &sdktls.TLSConfig{MinVersion: minVersion}
	if cipherSuites := strings.TrimSpace(configMap.Data[sdktls.ConfigMapKeyCipherSuites]); cipherSuites != "" {
		supported, unsupported := sdktls.ParseCipherSuites(cipherSuites)
		if len(unsupported) > 0 {
			klog.Warningf("Unsupported cipher suites in ConfigMap %s/%s: %v", configMap.Namespace, configMap.Name, unsupported)
			if len(supported) == 0 {
				return nil, true, fmt.Errorf("invalid cipherSuites in ConfigMap %s/%s: no supported cipher suites found",
					configMap.Namespace, configMap.Name)
			}
		}
		profile.CipherSuites = supported
	}
	return profile, false, nil
}

func jitterDuration(duration time.Duration) time.Duration {
	// A deterministic source is unnecessary here: process scheduling and watch
	// delivery already decorrelate replicas, while this bounded timestamp jitter
	// avoids synchronized retry waves after an API outage.
	nanos := time.Now().UnixNano()
	spread := duration / 5
	if spread <= 0 {
		return duration
	}
	return duration - spread + time.Duration(nanos%int64(2*spread+1))
}

func cloneTLSProfile(cfg *sdktls.TLSConfig) *sdktls.TLSConfig {
	if cfg == nil {
		return nil
	}
	return &sdktls.TLSConfig{
		MinVersion:   cfg.MinVersion,
		CipherSuites: append([]uint16(nil), cfg.CipherSuites...),
	}
}
