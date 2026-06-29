/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"net"
	"strings"
)

// parseServiceHostname extracts Kubernetes Service name and namespace from a
// cluster DNS hostname of the form:
//
//	<service>.<namespace>.svc
//	<service>.<namespace>.svc.cluster.local
//
// It returns ("", "") when the host does not match that pattern or is an IP address.
func parseServiceHostname(host string) (name, namespace string) {
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if host == "" {
		return "", ""
	}
	if net.ParseIP(host) != nil {
		return "", ""
	}
	parts := strings.Split(host, ".")
	if len(parts) < 3 {
		return "", ""
	}
	// <name>.<ns>.svc
	if len(parts) == 3 && parts[2] == "svc" {
		return parts[0], parts[1]
	}
	// <name>.<ns>.svc.cluster.local
	if len(parts) >= 5 && parts[2] == "svc" && parts[3] == "cluster" && parts[4] == "local" {
		return parts[0], parts[1]
	}
	return "", ""
}
