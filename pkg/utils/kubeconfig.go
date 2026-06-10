package utils

import (
	"fmt"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// CurrentCluster returns the cluster referenced by the kubeconfig's current
// context, falling back to the sole cluster when no context selects one.
func CurrentCluster(config *clientcmdapi.Config) (*clientcmdapi.Cluster, error) {
	if config == nil {
		return nil, fmt.Errorf("kubeconfig is empty")
	}
	return currentEntry("cluster", contextName(config, func(c *clientcmdapi.Context) string { return c.Cluster }), config.Clusters)
}

// CurrentAuthInfo returns the authinfo referenced by the kubeconfig's current
// context, falling back to the sole authinfo when no context selects one.
func CurrentAuthInfo(config *clientcmdapi.Config) (*clientcmdapi.AuthInfo, error) {
	if config == nil {
		return nil, fmt.Errorf("kubeconfig is empty")
	}
	return currentEntry("authinfo", contextName(config, func(c *clientcmdapi.Context) string { return c.AuthInfo }), config.AuthInfos)
}

// contextName returns the entry name selected by the kubeconfig's current
// context, or "" when no current context is set or it resolves to no name.
func contextName(config *clientcmdapi.Config, name func(*clientcmdapi.Context) string) string {
	if config.CurrentContext == "" {
		return ""
	}
	if context, ok := config.Contexts[config.CurrentContext]; ok {
		return name(context)
	}
	return ""
}

// currentEntry looks up name in entries, falling back to the sole entry when
// name is empty. kind names the entry type for error messages.
func currentEntry[T any](kind, name string, entries map[string]*T) (*T, error) {
	if name != "" {
		entry, ok := entries[name]
		if !ok {
			return nil, fmt.Errorf("current context references missing %s %q", kind, name)
		}
		return entry, nil
	}
	if len(entries) == 1 {
		for _, entry := range entries {
			return entry, nil
		}
	}
	return nil, fmt.Errorf("kubeconfig must have a current context or exactly one %s", kind)
}
