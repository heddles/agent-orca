package tenancy

import (
	"os"
	"slices"
	"strings"
	"sync/atomic"
)

const (
	// PlatformNamespacesEnvVar lists the platform catalog namespaces, comma
	// separated. When it is empty the operator's own namespace (POD_NAMESPACE)
	// is the only one.
	PlatformNamespacesEnvVar = "PLATFORM_NAMESPACES"
	podNamespaceEnvVar       = "POD_NAMESPACE"
)

var platformNamespaces atomic.Value // []string

// SetPlatformNamespaces sets the platform catalog namespaces: places where the
// platform team keeps shared objects, such as agent templates. In them, an
// object labelled agentorca.io/scope=platform is readable by every tenant
// whatever its allowed namespaces. Every other object in them stays hidden from
// tenants that do not claim the namespace, and platform objects stay read only.
func SetPlatformNamespaces(ns []string) {
	clean := make([]string, 0, len(ns))
	for _, n := range ns {
		if n = strings.TrimSpace(n); n != "" && !slices.Contains(clean, n) {
			clean = append(clean, n)
		}
	}
	platformNamespaces.Store(clean)
}

// SetPlatformNamespacesFromEnv reads PlatformNamespacesEnvVar, defaulting to
// the operator's own namespace.
func SetPlatformNamespacesFromEnv() {
	if v := os.Getenv(PlatformNamespacesEnvVar); strings.TrimSpace(v) != "" {
		SetPlatformNamespaces(strings.Split(v, ","))
		return
	}
	SetPlatformNamespaces([]string{os.Getenv(podNamespaceEnvVar)})
}

// PlatformNamespaces returns a copy of the platform catalog namespaces.
func PlatformNamespaces() []string {
	list, _ := platformNamespaces.Load().([]string)
	return slices.Clone(list)
}

// IsPlatformNamespace reports whether ns is a platform catalog namespace.
func IsPlatformNamespace(ns string) bool {
	list, _ := platformNamespaces.Load().([]string)
	return ns != "" && slices.Contains(list, ns)
}
