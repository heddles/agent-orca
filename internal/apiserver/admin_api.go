/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package apiserver

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"log/slog"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/floppyfish14/agent-orca/api/v1alpha1"
)

// tenantAuthModeIssued is the TenantConfig authMode value for issued JWTs.
const tenantAuthModeIssued = "issued"

// Admin API types (port 8084, /admin/*).

// AdminRateLimit mirrors TenantRateLimit for the create request.
type AdminRateLimit struct {
	RequestsPerMinute int `json:"requestsPerMinute,omitempty"`
	ConcurrentRuns    int `json:"concurrentRuns,omitempty"`
}

// AdminTenantCreateRequest is the body for POST /admin/tenants.
type AdminTenantCreateRequest struct {
	Name            string          `json:"name"`
	TargetNamespace string          `json:"targetNamespace"`
	ClientID        string          `json:"clientID"`
	ClientSecret    string          `json:"clientSecret,omitempty"`
	AllowedAgents   []string        `json:"allowedAgents,omitempty"`
	RateLimit       *AdminRateLimit `json:"rateLimit,omitempty"`
	BudgetPerDayUSD string          `json:"budgetPerDayUSD,omitempty"`
}

// AdminTenantResponse is the tenant representation returned to callers.
// ClientSecret is included ONLY at create time (one-time disclosure).
type AdminTenantResponse struct {
	Name            string          `json:"name"`
	Namespace       string          `json:"namespace"`
	ClientID        string          `json:"clientID"`
	ClientSecret    string          `json:"clientSecret,omitempty"`
	TargetNamespace string          `json:"targetNamespace"`
	AllowedAgents   []string        `json:"allowedAgents,omitempty"`
	RateLimit       *AdminRateLimit `json:"rateLimit,omitempty"`
	BudgetPerDayUSD string          `json:"budgetPerDayUSD,omitempty"`
}

// saUsernameParts parses a Kubernetes TokenReview username of the form
// "system:serviceaccount:<namespace>:<name>" into its namespace and name. It
// returns ok=false for non-SA identities (e.g. OIDC id_tokens), so callers can
// reject them without duplicating the shape check. Shared by requireAdminAuth
// and requireSAOrSelfTenant to keep the SA-username contract in one place.
func saUsernameParts(username string) (namespace, name string, ok bool) {
	parts := strings.Split(username, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" {
		return "", "", false
	}
	return parts[2], parts[3], true
}

// adminRequiresSAHint is the 401 message returned when a non-ServiceAccount
// caller (e.g. an OIDC id_token) hits a full-admin /admin/* operation. It tells
// them which token type is required and what their OIDC token CAN do, instead
// of a bare "unauthorized".
const adminRequiresSAHint = "unauthorized: list/create/delete/rotate-secret on /admin/* " +
	"require a Kubernetes ServiceAccount token carrying agentorca.io/admin=true; " +
	"an OIDC id_token may only read its own tenant via GET /admin/tenants/<name>"

// requireAdminAuth gates the /admin/* surface: the caller must present a valid
// Kubernetes ServiceAccount token whose SA carries the
// `agentorca.io/admin: "true"` label.
func (s *ExternalAPIServer) requireAdminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := extractBearer(r)
		if token == "" {
			writeAuthFailureJSON(w, "missing Authorization header")
			return
		}
		username, ok, err := s.reviewSAToken(r.Context(), token)
		if err != nil {
			slog.Warn("admin token review failed", "path", r.URL.Path, "err", err)
			writeAuthFailureJSON(w, "unauthorized")
			return
		}
		if !ok {
			// Not an authenticated ServiceAccount — typically an OIDC id_token or
			// an issued tenant JWT. Point the caller at the SA-token path (and the
			// one OIDC-allowed op) instead of a bare "unauthorized".
			writeAuthFailureJSON(w, adminRequiresSAHint)
			return
		}
		ns, saName, saOK := saUsernameParts(username)
		if !saOK {
			slog.Warn("admin SA username has unexpected shape", "username", username)
			writeAuthFailureJSON(w, "unauthorized")
			return
		}
		allowed, err := s.isAdminSA(r.Context(), ns, saName)
		if err != nil {
			slog.Warn("admin SA check failed", "namespace", ns, "sa", saName, "err", err)
			writeAuthFailureJSON(w, "unauthorized")
			return
		}
		if !allowed {
			http.Error(w, `{"error":"forbidden: ServiceAccount must carry the agentorca.io/admin=true label"}`, http.StatusForbidden)
			return
		}
		// Stash the admin identity for downstream handlers (audit/logging).
		r = r.WithContext(withAdminIdentity(r.Context(), adminIdentity{Namespace: ns, Name: saName}))
		next.ServeHTTP(w, r)
	})
}

// requireSAOrSelfTenant grants access on the admin surface to either:
//   - an admin ServiceAccount token (full, unchanged access), or
//   - a federated OIDC id_token belonging to the caller's OWN federated tenant,
//     for one of the self-tenant capabilities in selfTenantAdminOps (today: only
//     GET /admin/tenants/<name> read, where <name> is the TenantConfig the
//     caller's id_token resolved to).
//
// This is the human-OIDC bridge that lets a `aoctl login --auth-method oidc` user
// read their own tenant config without a service-account token, ahead of
// per-tenant RBAC (group/role mapping). A valid token aimed at a different
// tenant returns 404 (to avoid leaking which tenants exist); a token the server
// cannot attribute to a SA or tenant is 401.
func (s *ExternalAPIServer) requireSAOrSelfTenant(name string, capability adminCapability, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := extractBearer(r)
		// 1) ServiceAccount admin token → full access (unchanged requirement).
		if username, ok, err := s.reviewSAToken(r.Context(), token); err == nil && ok {
			if saNS, saName, saOK := saUsernameParts(username); saOK {
				if allowed, _ := s.isAdminSA(r.Context(), saNS, saName); allowed {
					r = r.WithContext(withAdminIdentity(r.Context(), adminIdentity{Namespace: saNS, Name: saName}))
					next.ServeHTTP(w, r)
					return
				}
			}
		}
		// 2) Federated id_token → scoped to the caller's OWN tenant. ValidateToken
		//    resolves a federated id_token to a TenantIdentity whose TenantName is
		//    the matched TenantConfig name.
		if ident, err := s.auth.ValidateToken(r.Context(), token); err == nil && ident != nil {
			// TODO(per-tenant-RBAC): once IdP groups are mapped to roles, gate the
			//   additional selfTenantAdminOps (e.g. rotate-secret) on
			//   capability.role and ident.Roles here; deny with 404 when the role is
			//   absent. Today capability.role is always empty, so any valid id_token
			//   for the caller's own tenant is admitted.
			if capability.role != "" && !identityHasRole(ident, capability.role) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
				return
			}
			if ident.TenantName != name {
				// Valid token, different tenant → 404 (do not reveal existence).
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
				return
			}
			r = r.WithContext(withAdminIdentity(r.Context(), adminIdentity{
				Namespace: ident.Namespace, Name: "oidc:" + ident.TenantName,
			}))
			next.ServeHTTP(w, r)
			return
		}
		// 3) No recognizable admin or federated token at all.
		writeAuthFailureJSON(w, "unauthorized")
	})
}

// identityHasRole reports whether identity carries the given IdP-derived role.
// Roles are populated from a tenant's group/role mapping; empty until the
// per-tenant RBAC layer lands. Used by the capability.role gate above.
func identityHasRole(ident *TenantIdentity, role string) bool {
	for _, r := range ident.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// --- admin identity context ---

type adminIdentity struct{ Namespace, Name string }

type adminCtxKey struct{}

func withAdminIdentity(ctx context.Context, id adminIdentity) context.Context {
	return context.WithValue(ctx, adminCtxKey{}, id)
}

// AdminFromContext returns the authenticated admin SA identity, if any.
func AdminFromContext(ctx context.Context) (adminIdentity, bool) {
	id, ok := ctx.Value(adminCtxKey{}).(adminIdentity)
	return id, ok
}

// --- route dispatch ---

// handleAdminTenants dispatches GET (list) and POST (create) for /admin/tenants.
func (s *ExternalAPIServer) handleAdminTenants(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listTenants(w, r)
	case http.MethodPost:
		s.createTenant(w, r)
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

// handleAdminTenantByID dispatches /admin/tenants/{name} (GET/DELETE) and
// /admin/tenants/{name}/rotate-secret (POST).
func (s *ExternalAPIServer) handleAdminTenantByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/admin/tenants/")
	name := path
	action := ""
	if before, after, ok := strings.Cut(path, "/"); ok {
		name, action = before, after
	}
	if name == "" {
		http.Error(w, `{"error":"tenant name required"}`, http.StatusBadRequest)
		return
	}

	switch {
	case r.Method == http.MethodGet && action == "":
		s.getTenant(w, r, name)
	case r.Method == http.MethodDelete && action == "":
		s.deleteTenant(w, r, name)
	case r.Method == http.MethodPost && action == "rotate-secret":
		s.rotateTenantSecret(w, r, name)
	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
}

// --- handlers ---

func (s *ExternalAPIServer) listTenants(w http.ResponseWriter, _ *http.Request) {
	var list agentorcav1alpha1.TenantConfigList
	if err := s.crdClient.List(context.Background(), &list); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"listing tenants: %s"}`, err), http.StatusInternalServerError)
		return
	}
	out := make([]AdminTenantResponse, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, tenantToResponse(&list.Items[i], ""))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"tenants": out, "count": len(out)})
}

func (s *ExternalAPIServer) getTenant(w http.ResponseWriter, _ *http.Request, name string) {
	var tc agentorcav1alpha1.TenantConfig
	if err := s.crdClient.Get(context.Background(), client.ObjectKey{Name: name, Namespace: s.adminNamespace()}, &tc); err != nil {
		code := http.StatusInternalServerError
		if k8serrors.IsNotFound(err) {
			code = http.StatusNotFound
		}
		http.Error(w, fmt.Sprintf(`{"error":"tenant %q not found: %s"}`, name, err), code)
		return
	}
	writeJSON(w, http.StatusOK, tenantToResponse(&tc, ""))
}

func (s *ExternalAPIServer) createTenant(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, `{"error":"reading body"}`, http.StatusBadRequest)
		return
	}
	var req AdminTenantCreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"invalid JSON: %s"}`, err), http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.TargetNamespace == "" || req.ClientID == "" {
		http.Error(w, `{"error":"name, targetNamespace, and clientID are required"}`, http.StatusBadRequest)
		return
	}

	ns := s.adminNamespace()

	// Ensure the client secret exists (create or update).
	// Convention: <tenant-name>-client-secret in the admin namespace.
	secretName := req.Name + "-client-secret"
	secretVal := req.ClientSecret
	if secretVal == "" {
		secretVal, err = generateSecret()
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"generating secret: %s"}`, err), http.StatusInternalServerError)
			return
		}
	}
	if err := s.upsertSecret(r.Context(), ns, secretName, secretVal); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"persisting secret: %s"}`, err), http.StatusInternalServerError)
		return
	}

	// Create the TenantConfig (idempotent: re-GET and report if it already exists).
	tc := &agentorcav1alpha1.TenantConfig{
		ObjectMeta: metav1.ObjectMeta{Name: req.Name, Namespace: ns},
		Spec: agentorcav1alpha1.TenantConfigSpec{
			AuthMode: tenantAuthModeIssued,
			Issued: &agentorcav1alpha1.IssuedAuthConfig{
				ClientID: req.ClientID,
				ClientSecretRef: agentorcav1alpha1.SecretKeyRef{
					Name: secretName,
					Key:  "client-secret",
				},
			},
			TargetNamespace: req.TargetNamespace,
			AllowedAgents:   req.AllowedAgents,
			RateLimit:       toSpecRateLimit(req.RateLimit),
			BudgetPerDayUSD: req.BudgetPerDayUSD,
		},
	}
	if err := s.crdClient.Create(r.Context(), tc); err != nil && !k8serrors.IsAlreadyExists(err) {
		http.Error(w, fmt.Sprintf(`{"error":"creating tenant: %s"}`, err), http.StatusInternalServerError)
		return
	}

	slog.Info("admin created tenant", "name", req.Name, "namespace", ns,
		"clientID", req.ClientID, "generatedSecret", req.ClientSecret == "")
	writeJSON(w, http.StatusCreated, tenantToResponse(tc, secretVal))
}

func (s *ExternalAPIServer) rotateTenantSecret(w http.ResponseWriter, r *http.Request, name string) {
	ctx := r.Context()
	var tc agentorcav1alpha1.TenantConfig
	if err := s.crdClient.Get(ctx, client.ObjectKey{Name: name, Namespace: s.adminNamespace()}, &tc); err != nil {
		code := http.StatusInternalServerError
		if k8serrors.IsNotFound(err) {
			code = http.StatusNotFound
		}
		http.Error(w, fmt.Sprintf(`{"error":"tenant %q not found: %s"}`, name, err), code)
		return
	}
	secretName := tc.Spec.Issued.ClientSecretRef.Name
	if secretName == "" {
		http.Error(w, `{"error":"tenant has no client secret to rotate"}`, http.StatusBadRequest)
		return
	}
	newVal, err := generateSecret()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"generating secret: %s"}`, err), http.StatusInternalServerError)
		return
	}
	if err := s.upsertSecret(ctx, s.adminNamespace(), secretName, newVal); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"persisting secret: %s"}`, err), http.StatusInternalServerError)
		return
	}
	slog.Info("admin rotated client secret", "tenant", name, "secret", secretName)
	writeJSON(w, http.StatusOK, map[string]string{"clientSecret": newVal})
}

func (s *ExternalAPIServer) deleteTenant(w http.ResponseWriter, r *http.Request, name string) {
	ctx := r.Context()
	var tc agentorcav1alpha1.TenantConfig
	if err := s.crdClient.Get(ctx, client.ObjectKey{Name: name, Namespace: s.adminNamespace()}, &tc); err != nil {
		code := http.StatusInternalServerError
		if k8serrors.IsNotFound(err) {
			code = http.StatusNotFound
		}
		http.Error(w, fmt.Sprintf(`{"error":"tenant %q not found: %s"}`, name, err), code)
		return
	}
	// Delete the client secret if it is managed by this tenant.
	if tc.Spec.Issued != nil && tc.Spec.Issued.ClientSecretRef.Name != "" {
		secKey := client.ObjectKey{Name: tc.Spec.Issued.ClientSecretRef.Name, Namespace: s.adminNamespace()}
		_ = s.crdClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secKey.Name, Namespace: secKey.Namespace}})
	}
	if err := s.crdClient.Delete(ctx, &tc); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"deleting tenant: %s"}`, err), http.StatusInternalServerError)
		return
	}
	slog.Info("admin deleted tenant", "name", name)
	w.WriteHeader(http.StatusNoContent)
}

// upsertSecret creates or updates the described client-secret value.
func (s *ExternalAPIServer) upsertSecret(ctx context.Context, namespace, name, value string) error {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       map[string][]byte{"client-secret": []byte(value)},
	}
	if err := s.crdClient.Create(ctx, sec); err != nil {
		if !k8serrors.IsAlreadyExists(err) {
			return err
		}
		// Update the existing secret's data.
		existing := &corev1.Secret{}
		if err := s.crdClient.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, existing); err != nil {
			return err
		}
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Data = map[string][]byte{"client-secret": []byte(value)}
		return s.crdClient.Patch(ctx, existing, patch)
	}
	return nil
}

// tenantToResponse converts a TenantConfig into the admin response. If secret is
// non-empty it is included (create/rotate); otherwise it is omitted.
func tenantToResponse(tc *agentorcav1alpha1.TenantConfig, secret string) AdminTenantResponse {
	resp := AdminTenantResponse{
		Name:            tc.Name,
		Namespace:       tc.Namespace,
		TargetNamespace: tc.Spec.TargetNamespace,
		AllowedAgents:   tc.Spec.AllowedAgents,
		BudgetPerDayUSD: tc.Spec.BudgetPerDayUSD,
	}
	if tc.Spec.Issued != nil {
		resp.ClientID = tc.Spec.Issued.ClientID
	}
	if rl := tc.Spec.RateLimit; rl != nil {
		resp.RateLimit = &AdminRateLimit{RequestsPerMinute: rl.RequestsPerMinute, ConcurrentRuns: rl.ConcurrentRuns}
	}
	if secret != "" {
		resp.ClientSecret = secret
	}
	return resp
}

// toSpecRateLimit nil-maps to nil (omitted).
func toSpecRateLimit(rl *AdminRateLimit) *agentorcav1alpha1.TenantRateLimit {
	if rl == nil {
		return nil
	}
	return &agentorcav1alpha1.TenantRateLimit{RequestsPerMinute: rl.RequestsPerMinute, ConcurrentRuns: rl.ConcurrentRuns}
}

// generateSecret returns a URL-safe random secret (32 bytes of entropy).
func generateSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// writeJSON encodes v as JSON with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Compile-time guard: the author assumed authv1 is needed for the documented
// audience; keep the import referenced via a type alias check.
