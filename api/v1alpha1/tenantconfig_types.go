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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TenantConfigSpec defines authentication and authorization for an enterprise tenant.
type TenantConfigSpec struct {
	// AuthMode selects the authentication strategy for this tenant.
	// "issued" — agent-orca issues OAuth2 tokens via client_credentials grant.
	// "federated" — agent-orca trusts JWTs from the tenant's external IdP.
	// +kubebuilder:validation:Enum=issued;federated
	AuthMode string `json:"authMode"`

	// Issued configures agent-orca-issued OAuth2 client credentials.
	// Required when AuthMode is "issued".
	// +optional
	Issued *IssuedAuthConfig `json:"issued,omitempty"`

	// Federated configures trust for an external OIDC identity provider.
	// Required when AuthMode is "federated".
	// +optional
	Federated *FederatedAuthConfig `json:"federated,omitempty"`

	// AllowedNamespaces is the set of Kubernetes namespaces whose agents and
	// resources this tenant is authorized to access. The operator resolves each
	// agent to the namespace it actually lives in — which must be one of these —
	// when a task is submitted or an ACP run is created, so a single tenant can
	// span multiple namespaces. List operations (agents, runs) span the whole set;
	// per-request writes are directed to the agent's namespace.
	// +kubebuilder:validation:MinItems=1
	AllowedNamespaces []string `json:"allowedNamespaces"`

	// AllowedAgents restricts which agents this tenant may invoke.
	// An empty list means no agents are allowed; omitting the field allows all agents
	// in the allowed namespaces.
	// +optional
	AllowedAgents []string `json:"allowedAgents,omitempty"`

	// RateLimit configures request rate limiting for this tenant.
	// +optional
	RateLimit *TenantRateLimit `json:"rateLimit,omitempty"`

	// BudgetPerDayUSD is the maximum daily spend in USD for this tenant.
	// Enforced across all runs in the allowed namespaces attributed to this tenant.
	// +optional
	BudgetPerDayUSD string `json:"budgetPerDayUSD,omitempty"`
}

// IssuedAuthConfig configures agent-orca as the OAuth2 token issuer.
// Enterprise customers use the client_credentials grant to obtain short-lived JWTs.
type IssuedAuthConfig struct {
	// ClientID is the OAuth2 client identifier for this tenant.
	// +kubebuilder:validation:MinLength=1
	ClientID string `json:"clientID"`

	// ClientSecretRef references the Kubernetes Secret containing the client secret.
	// The operator reads this to validate client_credentials requests.
	ClientSecretRef SecretKeyRef `json:"clientSecretRef"`
}

// FederatedAuthConfig configures trust for an external OIDC identity provider.
// agent-orca validates JWTs issued by the tenant's IdP and maps claims to tenant identity.
//
// Bearer/federated trust (no browser login) needs only IssuerURL + ClientID (+ optional
// matchClaim/matchValue); agent-orca verifies caller-presented JWTs against the IdP's
// public JWKS. The interactive (authorization-code) browser-login flow additionally
// needs ClientSecretRef + RedirectURI + ClaimMappings, all of which may be omitted on
// a bearer-only tenant.
type FederatedAuthConfig struct {
	// IssuerURL is the OIDC issuer URL (e.g. "https://acme.okta.com/oauth2/default").
	// agent-orca fetches the JWKS from this issuer to verify token signatures.
	// +kubebuilder:validation:MinLength=1
	IssuerURL string `json:"issuerURL"`

	// ClientID is the expected "aud" (audience) claim in the JWT for bearer tokens,
	// and the OAuth2 client ID used for the interactive login flow.
	// +kubebuilder:validation:MinLength=1
	ClientID string `json:"clientID"`

	// MatchClaim is the JWT claim used to identify this tenant (e.g. "org_id",
	// "tenant", or a GitHub-style "repository_owner"). When both MatchClaim and
	// MatchValue are omitted the tenant trusts *any* token validly signed by
	// IssuerURL with the expected ClientID audience (issuer-only / default-allow).
	// +optional
	// +kubebuilder:validation:MinLength=1
	MatchClaim string `json:"matchClaim,omitempty"`

	// MatchValue is the expected value of MatchClaim that maps to this tenant.
	// Required when MatchClaim is set; omit both for issuer-only trust.
	// +optional
	// +kubebuilder:validation:MinLength=1
	MatchValue string `json:"matchValue,omitempty"`

	// ClientSecretRef references the OAuth2 client_secret used for the interactive
	// login (authorization-code) flow. When the ref omits a namespace, the
	// operator looks it up in the tenant's first allowed namespace
	// (spec.allowedNamespaces[0]). Omit for bearer-only federation (no secret is
	// needed — verification uses the IdP's public JWKS).
	// +optional
	ClientSecretRef SecretKeyRef `json:"clientSecretRef,omitempty"`

	// RedirectURI is the callback URL registered with the IdP for the interactive
	// login flow. Omit for bearer-only federation.
	// +optional
	RedirectURI string `json:"redirectURI,omitempty"`

	// ClaimMappings selects which IdP claims map onto the resolved principal/identity.
	// All fields default to the standard OIDC claim when empty (userid=sub,
	// groups=groups, email=email). Set userid=email for Google social login.
	// +optional
	ClaimMappings FederatedClaimMappings `json:"claimMappings,omitempty"`

	// Scopes configures the OAuth2 scopes requested during the interactive
	// (authorization-code) login flow. When omitted, defaults to
	// ["openid","email","profile"]. Some IdPs require additional scopes — notably
	// "offline_access" to receive a refresh token (so the session can be renewed
	// without re-prompting for credentials). Has no effect on bearer-only
	// federation: agent-orca verifies caller-presented JWTs against the IdP's
	// JWKS regardless of scopes.
	// +optional
	Scopes []string `json:"scopes,omitempty"`
}

// FederatedClaimMappings maps IdP claims onto TenantIdentity fields. Zero values
// mean "use the standard default" (resolved by the OIDC provider config).
type FederatedClaimMappings struct {
	// UserID maps to TenantIdentity.UserID (default "sub").
	// +optional
	UserID string `json:"userid,omitempty"`
	// Groups maps to TenantIdentity.Groups (default "groups").
	// +optional
	Groups string `json:"groups,omitempty"`
	// Email maps to TenantIdentity.Email (default "email").
	// +optional
	Email string `json:"email,omitempty"`
}

// TenantRateLimit configures request rate limiting.
type TenantRateLimit struct {
	// RequestsPerMinute is the maximum number of task submissions per minute.
	// +optional
	RequestsPerMinute int `json:"requestsPerMinute,omitempty"`

	// ConcurrentRuns is the maximum number of simultaneously running AgentRuns.
	// +optional
	ConcurrentRuns int `json:"concurrentRuns,omitempty"`
}

// TenantConfigStatus holds the observed state of a TenantConfig.
type TenantConfigStatus struct {
	// Ready indicates the tenant configuration has been validated and is active.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// Message contains a human-readable status message.
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions holds standard Kubernetes conditions.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="AuthMode",type=string,JSONPath=`.spec.authMode`
// +kubebuilder:printcolumn:name="Namespaces",type=string,JSONPath=`.spec.allowedNamespaces`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// TenantConfig defines authentication and authorization for an enterprise tenant
// accessing agent-orca's external API.
type TenantConfig struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TenantConfigSpec   `json:"spec,omitempty"`
	Status TenantConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TenantConfigList contains a list of TenantConfig.
type TenantConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TenantConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&TenantConfig{}, &TenantConfigList{})
}
