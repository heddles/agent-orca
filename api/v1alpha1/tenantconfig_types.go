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
	// TenantID pins the tenant's stable identifier ("tnt_" plus a UUIDv7). Omit
	// it and the operator generates one at creation (see status.tenantID). Pin
	// it for a deterministic ID in GitOps, or to restore a deleted tenant's data
	// after disaster recovery: only a platform admin may pin the ID of a deleted
	// tenant. Once set it cannot change.
	// +optional
	// +kubebuilder:validation:Pattern=`^tnt_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`
	TenantID string `json:"tenantID,omitempty"`

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

	// AllowedAgents restricts which agents this tenant may invoke. With no agents
	// listed (the field omitted or empty), every agent in the allowed namespaces is
	// allowed. With agents listed, only those are allowed.
	// +optional
	AllowedAgents []string `json:"allowedAgents,omitempty"`

	// RateLimit configures request rate limiting for this tenant.
	// +optional
	RateLimit *TenantRateLimit `json:"rateLimit,omitempty"`

	// BudgetPerDayUSD is the maximum daily spend in USD for this tenant.
	// Enforced across all runs in the allowed namespaces attributed to this tenant.
	// +optional
	BudgetPerDayUSD string `json:"budgetPerDayUSD,omitempty"`

	// Roles maps OIDC groups to roles inside this tenant. A signed-in user gets
	// the highest role any of their groups maps to, plus every role below it
	// (admin includes builder, builder includes runner, runner includes viewer).
	// Omit it and every signed-in user is a runner, which is how tenants behaved
	// before roles existed. Once it is set, a user in none of the listed groups
	// gets defaultRole, or no access at all when that is unset.
	// +optional
	Roles *TenantRoles `json:"roles,omitempty"`

	// Notifications are the places the tenant's schedules can send a message
	// to. Administrators own this field: it is not writable from the UI, which
	// only lists the channel names.
	// +optional
	Notifications *TenantNotifications `json:"notifications,omitempty"`

	// Dispatch controls how an agent is picked for a request that names none
	// ("auto"). Omit it and automatic routing is on, using the cluster's local
	// decision service when one is deployed.
	// +optional
	Dispatch *TenantDispatch `json:"dispatch,omitempty"`
}

// Deciders of TenantDispatch.
const (
	// DispatchDeciderLocal is the cluster's own decision service (orca-decide).
	DispatchDeciderLocal = "local"
	// DispatchDeciderModelSelector is one of the tenant's ModelSelectors.
	DispatchDeciderModelSelector = "modelSelector"
)

// TenantDispatch is how the tenant's requests that name no agent are routed. A
// request only ever goes to an agent its caller may already run.
// +kubebuilder:validation:XValidation:rule="!((has(self.decider) && self.decider == 'modelSelector') || (has(self.escalate) && self.escalate)) || has(self.modelSelectorRef)",message="modelSelectorRef is required when decider is modelSelector or escalate is true"
type TenantDispatch struct {
	// Enabled turns automatic routing on or off for the tenant. Default true.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Decider picks the agent. "local" is the cluster's decision service:
	// nothing leaves the cluster. "modelSelector" asks a model of
	// modelSelectorRef, with the tenant's own providers, credentials and budget;
	// the request and the agents' descriptions are sent to that provider.
	// +kubebuilder:validation:Enum=local;modelSelector
	// +kubebuilder:default=local
	// +optional
	Decider string `json:"decider,omitempty"`

	// ModelSelectorRef is the ModelSelector asked by the modelSelector decider,
	// or by escalate.
	// +optional
	ModelSelectorRef *DispatchModelSelectorRef `json:"modelSelectorRef,omitempty"`

	// Escalate, with the local decider, also asks modelSelectorRef when the
	// local one is unsure, before falling back to the default agent. Those
	// requests are sent to that provider and cost money. Default false.
	// +optional
	Escalate bool `json:"escalate,omitempty"`

	// DefaultAgent takes every request the decider is not sure about. A tenant
	// should set it: without it, an unsure request goes to the best candidate
	// only if that scores at least 50 percent, and is otherwise refused.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	DefaultAgent string `json:"defaultAgent,omitempty"`

	// MinConfidencePercent is how sure the decider must be for its pick to be
	// taken. Default 80.
	// +kubebuilder:validation:Minimum=50
	// +kubebuilder:validation:Maximum=100
	// +optional
	MinConfidencePercent *int32 `json:"minConfidencePercent,omitempty"`
}

// DispatchModelSelectorRef names a ModelSelector.
type DispatchModelSelectorRef struct {
	// Name of the ModelSelector.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Namespace of the ModelSelector. Defaults to the tenant's first allowed
	// namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// Notification channel types.
const (
	// NotificationChannelWebhook POSTs a JSON body signed with HMAC-SHA256.
	NotificationChannelWebhook = "webhook"
	// NotificationChannelSlack POSTs {"text": ...} to a Slack incoming webhook.
	NotificationChannelSlack = "slack"
	// NotificationChannelSMTP sends an email.
	NotificationChannelSMTP = "smtp"
)

// TenantNotifications lists the tenant's notification channels.
type TenantNotifications struct {
	// Channels are the named destinations an AgentSchedule's
	// spec.result.notify.channel can pick.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=20
	// +optional
	Channels []NotificationChannel `json:"channels,omitempty"`
}

// NotificationChannel is one destination. Where it goes is in a Secret, never
// in the spec: the URL of a Slack incoming webhook is itself a credential.
type NotificationChannel struct {
	// Name is what an AgentSchedule refers to.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// Type is webhook, slack or smtp.
	// +kubebuilder:validation:Enum=webhook;slack;smtp
	Type string `json:"type"`

	// SecretRef is the Secret that holds the destination.
	//
	//   - webhook: url, and signingKey (the HMAC-SHA256 key; the body is signed
	//     as "<timestamp>.<body>" like the trigger webhooks, in the headers
	//     X-AgentOrca-Timestamp and X-AgentOrca-Signature).
	//   - slack: url (an incoming-webhook URL).
	//   - smtp: host, port, from and to (comma separated), and optionally
	//     username and password.
	SecretRef NotificationSecretRef `json:"secretRef"`

	// Insecure allows an http:// URL (webhook, slack) or an SMTP server that
	// does not offer STARTTLS. Default false: https and STARTTLS are required.
	// +optional
	Insecure bool `json:"insecure,omitempty"`

	// AllowPrivateAddresses allows a destination that resolves to a loopback,
	// link-local, private or otherwise internal address, such as a mail relay
	// in the cluster. Default false: such addresses are refused, which keeps a
	// channel from being pointed at the cluster's own services or at a cloud
	// metadata endpoint.
	// +optional
	AllowPrivateAddresses bool `json:"allowPrivateAddresses,omitempty"`
}

// NotificationSecretRef names a Secret.
type NotificationSecretRef struct {
	// Name of the Secret.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Namespace of the Secret. Defaults to the TenantConfig's namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// TenantRoles maps OIDC groups to the roles of the UI API, ACP and External
// Task API inside one tenant.
//
//   - viewer: read.
//   - runner: viewer, plus start, cancel and chat with runs, answer
//     clarifications and send feedback.
//   - builder: runner, plus create, update and delete resources.
//   - admin: builder, plus model providers, model selectors, MCP servers, tools
//     and approval decisions, and the audit log.
type TenantRoles struct {
	// Viewer lists the groups that get the viewer role.
	// +optional
	Viewer []string `json:"viewer,omitempty"`
	// Runner lists the groups that get the runner role.
	// +optional
	Runner []string `json:"runner,omitempty"`
	// Builder lists the groups that get the builder role.
	// +optional
	Builder []string `json:"builder,omitempty"`
	// Admin lists the groups that get the admin role.
	// +optional
	Admin []string `json:"admin,omitempty"`
	// DefaultRole is the role of a signed-in user who is in none of the groups
	// above. Unset means such a user has no access.
	// +optional
	// +kubebuilder:validation:Enum=viewer;runner;builder;admin
	DefaultRole string `json:"defaultRole,omitempty"`
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
	// TenantID mirrors the tenant registry's ID for this TenantConfig. The
	// registry is the source of truth; this copy lets controllers and kubectl
	// read the ID without a database round trip.
	// +optional
	TenantID string `json:"tenantID,omitempty"`

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
// +kubebuilder:printcolumn:name="TenantID",type=string,JSONPath=`.status.tenantID`
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
