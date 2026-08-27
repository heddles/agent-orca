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
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang-jwt/jwt/v5"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/floppyfish14/agent-orca/api/v1alpha1"
)

const (
	// ExternalAPITokenAudience is the audience for K8s SA tokens on the external API.
	ExternalAPITokenAudience = "agentorca/external-api"

	// jwtIssuer is the issuer claim for agent-orca-issued JWTs.
	jwtIssuer = "agentorca"

	// jwtDefaultExpiry is the default expiry for issued tokens.
	jwtDefaultExpiry = 1 * time.Hour
)

// TenantIdentity is the resolved identity of an authenticated external API caller.
type TenantIdentity struct {
	// TenantName is the TenantConfig CR name.
	TenantName string
	// Namespace is the target Kubernetes namespace for this tenant.
	Namespace string
	// AllowedAgents is the list of agents this tenant may invoke. Nil means all.
	AllowedAgents []string

	// Quota fields below are populated from the tenant's TenantConfig (if any) and
	// are only set for issued/federated tenants — never for in-cluster K8s SA callers,
	// which assert their own namespace identity and are not rate/budget limited here.
	//
	// RateLimitRPM is RequestsPerMinute from TenantConfig.spec.rateLimit.
	RateLimitRPM int
	// ConcurrentRuns is spec.rateLimit.concurrentRuns.
	ConcurrentRuns int
	// BudgetPerDayUSD is spec.budgetPerDayUSD.
	BudgetPerDayUSD string
}

// contextKey is an unexported type for context keys to avoid collisions.
type contextKey int

const tenantIdentityKey contextKey = iota

// TenantFromContext retrieves the TenantIdentity from the request context.
func TenantFromContext(ctx context.Context) (*TenantIdentity, bool) {
	t, ok := ctx.Value(tenantIdentityKey).(*TenantIdentity)
	return t, ok
}

// ExternalAuth handles authentication for the external API.
// It supports three modes checked in order:
//  1. agent-orca-issued JWTs (for authMode: issued tenants)
//  2. Federated OIDC JWTs (for authMode: federated tenants)
//  3. Kubernetes ServiceAccount tokens (for in-cluster callers)
type ExternalAuth struct {
	k8s       kubernetes.Interface
	crdClient client.Client

	// signingKey is the RSA private key used to sign issued tokens.
	signingKey *rsa.PrivateKey

	// mu protects tenantCache, tenantByName, and oidcVerifiers.
	mu            sync.RWMutex
	tenantCache   map[string]*agentorcav1alpha1.TenantConfig // clientID -> TenantConfig
	tenantByName  map[string]*agentorcav1alpha1.TenantConfig // TenantConfig.name -> TenantConfig
	oidcVerifiers map[string]*gooidc.IDTokenVerifier         // issuerURL -> verifier

	// rateLimiter enforces per-tenant request rate limits on token submission.
	rateLimiter *RateLimiter
}

const (
	// signingKeySecretName is the name of the Kubernetes Secret used to persist the
	// JWT signing key across operator restarts.
	signingKeySecretName = "agentorca-jwt-signing-key"
	signingKeySecretKey  = "private-key.pem"
)

// NewExternalAuth creates a new ExternalAuth middleware.
// The signing key is loaded from a Kubernetes Secret if it exists, otherwise a new
// key is generated and persisted so that issued tokens survive operator restarts.
func NewExternalAuth(k8s kubernetes.Interface, crdClient client.Client) (*ExternalAuth, error) {
	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		namespace = "agent-orca-system" //nolint:goconst

	}

	key, err := loadOrCreateSigningKey(context.Background(), k8s, namespace)
	if err != nil {
		return nil, fmt.Errorf("initializing signing key: %w", err)
	}

	return &ExternalAuth{
		k8s:           k8s,
		crdClient:     crdClient,
		signingKey:    key,
		tenantCache:   make(map[string]*agentorcav1alpha1.TenantConfig),
		tenantByName:  make(map[string]*agentorcav1alpha1.TenantConfig),
		oidcVerifiers: make(map[string]*gooidc.IDTokenVerifier),
		rateLimiter:   NewRateLimiter(),
	}, nil
}

// loadOrCreateSigningKey loads an RSA private key from the named Secret. If the
// Secret does not exist, a new 2048-bit key is generated, stored, and returned.
func loadOrCreateSigningKey(ctx context.Context, k8s kubernetes.Interface, namespace string) (*rsa.PrivateKey, error) {
	secret, err := k8s.CoreV1().Secrets(namespace).Get(ctx, signingKeySecretName, metav1.GetOptions{})
	if err == nil {
		// Secret exists — parse the PEM-encoded private key.
		pemData, ok := secret.Data[signingKeySecretKey]
		if ok && len(pemData) > 0 {
			block, _ := pem.Decode(pemData)
			if block != nil {
				key, parseErr := x509.ParsePKCS8PrivateKey(block.Bytes)
				if parseErr == nil {
					if rsaKey, ok := key.(*rsa.PrivateKey); ok {
						slog.Info("Loaded JWT signing key from Secret", "secret", signingKeySecretName)
						return rsaKey, nil
					}
				}
				slog.Warn("Failed to parse signing key from Secret, generating new key", "err", parseErr)
			}
		}
	} else if !k8serrors.IsNotFound(err) {
		return nil, fmt.Errorf("fetching signing key secret: %w", err)
	}

	// Generate a new key and persist it.
	newKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generating signing key: %w", err)
	}

	derBytes, err := x509.MarshalPKCS8PrivateKey(newKey)
	if err != nil {
		return nil, fmt.Errorf("marshaling signing key: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: derBytes})

	newSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      signingKeySecretName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "agent-orca",
				"app.kubernetes.io/component":  "jwt-signing-key",
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			signingKeySecretKey: pemBytes,
		},
	}
	if _, createErr := k8s.CoreV1().Secrets(namespace).Create(ctx, newSecret, metav1.CreateOptions{}); createErr != nil {
		if k8serrors.IsAlreadyExists(createErr) {
			// Race with another replica — reload.
			return loadOrCreateSigningKey(ctx, k8s, namespace)
		}
		return nil, fmt.Errorf("persisting signing key secret: %w", createErr)
	}

	slog.Info("Generated and persisted new JWT signing key", "secret", signingKeySecretName)
	return newKey, nil
}

// RefreshTenants reloads all TenantConfig CRs into the cache.
// Called by the TenantConfig controller on reconcile.
func (a *ExternalAuth) RefreshTenants(ctx context.Context) error {
	var list agentorcav1alpha1.TenantConfigList
	if err := a.crdClient.List(ctx, &list); err != nil {
		return fmt.Errorf("listing TenantConfigs: %w", err)
	}

	cache := make(map[string]*agentorcav1alpha1.TenantConfig, len(list.Items))
	byName := make(map[string]*agentorcav1alpha1.TenantConfig, len(list.Items))
	for i := range list.Items {
		tc := &list.Items[i]
		byName[tc.Name] = tc
		switch tc.Spec.AuthMode {
		case "issued":
			if tc.Spec.Issued != nil {
				cache["issued:"+tc.Spec.Issued.ClientID] = tc
			}
		case "federated":
			if tc.Spec.Federated != nil {
				cache["federated:"+tc.Spec.Federated.IssuerURL+":"+tc.Spec.Federated.MatchValue] = tc
			}
		}
	}

	a.mu.Lock()
	a.tenantCache = cache
	a.tenantByName = byName
	// Reset OIDC verifiers so they are re-created with potentially updated config.
	a.oidcVerifiers = make(map[string]*gooidc.IDTokenVerifier)
	a.mu.Unlock()

	slog.Info("Refreshed tenant cache", "count", len(cache))
	return nil
}

// Middleware returns an http.Handler that authenticates requests and injects TenantIdentity.
func (a *ExternalAuth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip auth for publicly-discoverable endpoints (token exchange,
		// OpenAPI contract, liveness probe). Resolving the set from a table
		// (rather than an else-if chain) keeps it consistent for every server
		// that shares this middleware.
		if publicPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}

		token := extractBearer(r)
		if token == "" {
			http.Error(w, `{"error":"missing Authorization header"}`, http.StatusUnauthorized)
			return
		}

		// Try agent-orca-issued JWT first.
		if identity, err := a.validateIssuedToken(token); err == nil {
			ctx := context.WithValue(r.Context(), tenantIdentityKey, identity)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		} else {
			slog.Debug("issued token validation failed", "path", r.URL.Path, "err", err)
		}

		// Try federated OIDC JWT.
		if identity, err := a.validateFederatedToken(r.Context(), token); err == nil {
			ctx := context.WithValue(r.Context(), tenantIdentityKey, identity)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		} else {
			slog.Warn("federated token validation failed", "path", r.URL.Path, "err", err)
		}

		// Fall back to Kubernetes SA token.
		if identity, err := a.validateK8sToken(r.Context(), token); err == nil {
			ctx := context.WithValue(r.Context(), tenantIdentityKey, identity)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		} else {
			slog.Debug("k8s token validation failed", "path", r.URL.Path, "err", err)
		}

		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	})
}

// ValidateToken attempts to validate a bearer token against all three supported
// auth modes and returns the resolved TenantIdentity. This is the single-token
// variant of Middleware, used by the UI API server as a fallback after K8s SA
// token validation fails (enabling OIDC tenant JWT login from the browser).
func (a *ExternalAuth) ValidateToken(ctx context.Context, token string) (*TenantIdentity, error) {
	// Try agent-orca-issued JWT first.
	if identity, err := a.validateIssuedToken(token); err == nil {
		return identity, nil
	} else {
		slog.Debug("issued token validation failed", "err", err)
	}

	// Try federated OIDC JWT.
	if identity, err := a.validateFederatedToken(ctx, token); err == nil {
		return identity, nil
	} else {
		slog.Debug("federated token validation failed", "err", err)
	}

	// Fall back to Kubernetes SA token.
	if identity, err := a.validateK8sToken(ctx, token); err == nil {
		return identity, nil
	} else {
		slog.Debug("k8s token validation failed", "err", err)
	}

	return nil, fmt.Errorf("unauthorized")
}

// HandleTokenRequest handles POST /oauth/token for client_credentials grant.
func (a *ExternalAuth) HandleTokenRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, `{"error":"invalid form data"}`, http.StatusBadRequest)
		return
	}

	grantType := r.FormValue("grant_type")
	if grantType != "client_credentials" {
		http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
		return
	}

	clientID := r.FormValue("client_id")
	clientSecret := r.FormValue("client_secret")
	if clientID == "" || clientSecret == "" {
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
		return
	}

	// Look up the tenant by client ID.
	a.mu.RLock()
	tc, ok := a.tenantCache["issued:"+clientID]
	a.mu.RUnlock()
	if !ok {
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
		return
	}

	// Validate client secret against the referenced Kubernetes Secret.
	secretNS := tc.Namespace
	if tc.Spec.Issued.ClientSecretRef.Namespace != "" {
		secretNS = tc.Spec.Issued.ClientSecretRef.Namespace
	}
	secret, err := a.k8s.CoreV1().Secrets(secretNS).Get(r.Context(),
		tc.Spec.Issued.ClientSecretRef.Name, metav1.GetOptions{})
	if err != nil {
		slog.Error("failed to fetch client secret", "tenant", tc.Name, "err", err)
		http.Error(w, `{"error":"server_error"}`, http.StatusInternalServerError)
		return
	}
	expectedSecret := string(secret.Data[tc.Spec.Issued.ClientSecretRef.Key])
	if clientSecret != expectedSecret {
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
		return
	}

	// Issue a signed JWT.
	now := time.Now()
	claims := jwt.MapClaims{
		"iss":            jwtIssuer,
		"sub":            tc.Name,
		"aud":            ExternalAPITokenAudience,
		"iat":            now.Unix(),
		"exp":            now.Add(jwtDefaultExpiry).Unix(),
		"tenant":         tc.Name,
		"namespace":      tc.Spec.TargetNamespace,
		"allowed_agents": tc.Spec.AllowedAgents,
	}
	jwtToken := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := jwtToken.SignedString(a.signingKey)
	if err != nil {
		slog.Error("failed to sign JWT", "err", err)
		http.Error(w, `{"error":"server_error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": signed,
		"token_type":   "Bearer",
		"expires_in":   int(jwtDefaultExpiry.Seconds()),
	})
}

// validateIssuedToken verifies an agent-orca-issued JWT.
func (a *ExternalAuth) validateIssuedToken(tokenString string) (*TenantIdentity, error) {
	token, err := jwt.Parse(tokenString, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return &a.signingKey.PublicKey, nil
	}, jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(jwtIssuer),
		jwt.WithAudience(ExternalAPITokenAudience))
	if err != nil {
		return nil, err
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token claims")
	}

	tenant, _ := claims["tenant"].(string)
	namespace, _ := claims["namespace"].(string)
	if tenant == "" || namespace == "" {
		return nil, fmt.Errorf("missing tenant or namespace claim")
	}

	var allowedAgents []string
	if agents, ok := claims["allowed_agents"].([]any); ok {
		for _, a := range agents {
			if s, ok := a.(string); ok {
				allowedAgents = append(allowedAgents, s)
			}
		}
	}

	return attachQuotaFields(&TenantIdentity{
		TenantName:    tenant,
		Namespace:     namespace,
		AllowedAgents: allowedAgents,
	}, a.tenantConfigFor(tenant)), nil
}

// getOrCreateVerifier returns a cached OIDC verifier for the given issuer, or creates
// one by fetching the provider's JWKS discovery document. Caller must NOT hold a.mu.
func (a *ExternalAuth) getOrCreateVerifier(ctx context.Context, issuerURL, clientID string) (*gooidc.IDTokenVerifier, error) {
	a.mu.RLock()
	v, ok := a.oidcVerifiers[issuerURL]
	a.mu.RUnlock()
	if ok {
		return v, nil
	}

	provider, err := gooidc.NewProvider(ctx, issuerURL)
	if err != nil {
		return nil, fmt.Errorf("discovering OIDC provider %q: %w", issuerURL, err)
	}
	verifier := provider.Verifier(&gooidc.Config{
		ClientID: clientID,
	})

	a.mu.Lock()
	a.oidcVerifiers[issuerURL] = verifier
	a.mu.Unlock()

	slog.Info("Created OIDC verifier", "issuer", issuerURL, "audience", clientID)
	return verifier, nil
}

// validateFederatedToken verifies a JWT from an external OIDC provider using
// the issuer's JWKS for cryptographic signature verification.
func (a *ExternalAuth) validateFederatedToken(ctx context.Context, tokenString string) (*TenantIdentity, error) {
	// Parse without verification to inspect the issuer claim only — we need
	// it to look up the matching TenantConfig before we can verify.
	parser := jwt.NewParser(jwt.WithoutClaimsValidation())
	token, _, err := parser.ParseUnverified(tokenString, jwt.MapClaims{})
	if err != nil {
		return nil, fmt.Errorf("parsing federated token: %w", err)
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, fmt.Errorf("invalid claims type")
	}

	issuer, _ := claims["iss"].(string)
	if issuer == "" || issuer == jwtIssuer {
		return nil, fmt.Errorf("not a federated token")
	}

	// Search for a matching TenantConfig.
	a.mu.RLock()
	var matchedTC *agentorcav1alpha1.TenantConfig
	for key, tc := range a.tenantCache {
		if !strings.HasPrefix(key, "federated:") {
			continue
		}
		fed := tc.Spec.Federated
		if fed == nil || fed.IssuerURL != issuer {
			continue
		}
		claimValue, _ := claims[fed.MatchClaim].(string)
		if claimValue != fed.MatchValue {
			continue
		}
		matchedTC = tc
		break
	}
	a.mu.RUnlock()

	if matchedTC == nil {
		return nil, fmt.Errorf("no matching federated tenant for issuer %q", issuer)
	}

	// Verify the token signature using the issuer's JWKS.
	fed := matchedTC.Spec.Federated
	verifier, err := a.getOrCreateVerifier(ctx, fed.IssuerURL, fed.ClientID)
	if err != nil {
		return nil, fmt.Errorf("getting OIDC verifier: %w", err)
	}
	if _, err := verifier.Verify(ctx, tokenString); err != nil {
		return nil, fmt.Errorf("OIDC token verification failed: %w", err)
	}

	return attachQuotaFields(&TenantIdentity{
		TenantName:    matchedTC.Name,
		Namespace:     matchedTC.Spec.TargetNamespace,
		AllowedAgents: matchedTC.Spec.AllowedAgents,
	}, matchedTC), nil
}

// attachQuotaFields copies the rate-limit and budget settings from a tenant's
// TenantConfig into the resolved identity so callers can enforce them. nil tc
// (e.g. in-cluster K8s SA callers) is tolerated and leaves quotas unset.
func attachQuotaFields(id *TenantIdentity, tc *agentorcav1alpha1.TenantConfig) *TenantIdentity {
	if id == nil {
		return id
	}
	id.RateLimitRPM = 0
	id.ConcurrentRuns = 0
	id.BudgetPerDayUSD = ""
	if tc == nil {
		return id
	}
	if rl := tc.Spec.RateLimit; rl != nil {
		id.RateLimitRPM = rl.RequestsPerMinute
		id.ConcurrentRuns = rl.ConcurrentRuns
	}
	id.BudgetPerDayUSD = tc.Spec.BudgetPerDayUSD
	return id
}

// tenantConfigFor returns the live TenantConfig for a tenant name, or nil.
func (a *ExternalAuth) tenantConfigFor(name string) *agentorcav1alpha1.TenantConfig {
	if a == nil || name == "" {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.tenantByName[name]
}

// validateK8sToken authenticates a Kubernetes ServiceAccount token.
func (a *ExternalAuth) validateK8sToken(ctx context.Context, token string) (*TenantIdentity, error) {
	tr := &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{
			Token:     token,
			Audiences: []string{ExternalAPITokenAudience},
		},
	}
	result, err := a.k8s.AuthenticationV1().TokenReviews().Create(ctx, tr, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("TokenReview failed: %w", err)
	}
	if !result.Status.Authenticated {
		return nil, fmt.Errorf("token not authenticated")
	}

	// Extract namespace from SA username: "system:serviceaccount:<ns>:<name>"
	parts := strings.Split(result.Status.User.Username, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" {
		return nil, fmt.Errorf("unexpected SA username format: %q", result.Status.User.Username)
	}

	return &TenantIdentity{
		TenantName:    parts[3], // SA name as tenant identifier
		Namespace:     parts[2],
		AllowedAgents: nil, // no restriction for in-cluster callers
	}, nil
}
