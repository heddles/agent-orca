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

package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/acpfs"
	"github.com/heddles/agent-orca/internal/apiserver"
	"github.com/heddles/agent-orca/internal/connections"
	"github.com/heddles/agent-orca/internal/controller"
	"github.com/heddles/agent-orca/internal/evals/judge"
	evalrunner "github.com/heddles/agent-orca/internal/evals/runner"
	"github.com/heddles/agent-orca/internal/githubapp"
	"github.com/heddles/agent-orca/internal/notify"
	"github.com/heddles/agent-orca/internal/oauth"
	"github.com/heddles/agent-orca/internal/openshell"
	"github.com/heddles/agent-orca/internal/podbuilder"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/schedule"
	"github.com/heddles/agent-orca/internal/security"
	"github.com/heddles/agent-orca/internal/state"
	"github.com/heddles/agent-orca/internal/tenancy"
	agentwebhook "github.com/heddles/agent-orca/internal/webhook"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(agentorcav1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var routingRecommendations bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	var routingEventsRetention time.Duration
	flag.DurationVar(&routingEventsRetention, "routing-events-retention", 30*24*time.Hour,
		"How long routing telemetry (one row per LLM call) is kept in PostgreSQL. 0 keeps it forever.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&routingRecommendations, "routing-recommendations", true,
		"Evaluate every ModelSelector's routing and write RoutingRecommendations (report only). "+
			"A selector opts out with the annotation agentorca.io/routing-recommendations=disabled.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	var internalAPICertPath, internalAPICertName, internalAPICertKey string
	flag.StringVar(&internalAPICertPath, "internal-api-cert-path", "",
		"The directory that contains the TLS certificate for internal API servers (ports 8082-8084). "+
			"When empty, internal APIs serve plain HTTP.")
	flag.StringVar(&internalAPICertName, "internal-api-cert-name", "tls.crt", "The name of the internal API certificate file.") //nolint:lll

	flag.StringVar(&internalAPICertKey, "internal-api-cert-key", "tls.key", "The name of the internal API key file.")
	var uiAuthEnabled bool
	flag.BoolVar(&uiAuthEnabled, "ui-auth-enabled", false,
		"Require a valid UIProxy Kubernetes SA token (audience 'agentorca/ui') on all UI API "+
			"requests. Set to true in production deployments where the UIProxy pod provides the token. "+
			"Leave false for local development (npm run dev + operator without the proxy).")
	var scheduleMinInterval time.Duration
	flag.DurationVar(&scheduleMinInterval, "schedule-min-interval", schedule.DefaultMinInterval,
		"The shortest gap an AgentSchedule may have between ticks.")
	var scheduleMaxFailures int
	flag.IntVar(&scheduleMaxFailures, "schedule-max-consecutive-failures", schedule.DefaultMaxConsecutiveFailures,
		"An AgentSchedule stops ticking after this many finished runs in a row failed, until it is resumed.")
	var adminBootstrapToken string
	flag.StringVar(&adminBootstrapToken, "admin-bootstrap-token", "",
		"One-time bootstrap: when set to a non-empty value, the operator creates a ServiceAccount "+
			"named 'agentorca-admin' (with the agentorca.io/admin=true label) in the operator namespace "+
			"and prints its long-lived bearer token to stdout once. Store this token like a kubeconfig "+
			"credential; use it to call POST /admin/tenants and other admin endpoints. Ignored on "+
			"subsequent restarts (the SA persists).")
	// OIDC interactive login (authorization-code flow) for browser login.
	// Off by default. When enabled, the UI API (:8083) serves /oauth/login,
	// /oauth/callback, /oauth/logout. Login is tenant-driven: each federated
	// TenantConfig carrying a ClientSecretRef is a selectable IdP, and the
	// callback builds the OIDC provider from that tenant issuer/client/secret
	// (read from the tenant's first allowed namespace) — so each org authenticates via
	// its own IdP. Per-tenant issuer/client_id/secret/redirect/claim mappings
	// live on the TenantConfig CRD; no operator-wide IdP client is configured.
	var oidcLoginEnabled bool
	flag.BoolVar(&oidcLoginEnabled, "oidc-login-enabled", false,
		"Enable interactive OIDC login (authorization-code flow) on the UI API"+
			" (/oauth/login, /oauth/callback, /oauth/logout). Per-tenant IdP config"+
			" lives on the TenantConfig CRD spec.federated.clientSecretRef.")
	var oidcCookieSecure bool
	flag.BoolVar(&oidcCookieSecure, "oidc-cookie-secure", true,
		"Set the Secure attribute on OIDC session cookies (enable behind TLS;"+
			" set false for plain-HTTP local dev).")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	// Env fallbacks: OIDC_LOGIN_ENABLED=true enables login; OIDC_COOKIE_SECURE
	// toggles the cookie Secure attribute (false for plain-HTTP local dev).
	if os.Getenv("OIDC_LOGIN_ENABLED") == "true" {
		oidcLoginEnabled = true
	}
	if os.Getenv("OIDC_COOKIE_SECURE") != "" {
		oidcCookieSecure = os.Getenv("OIDC_COOKIE_SECURE") == "true"
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.1/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.1/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "6950eef6.agentorca.io",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	// Build raw Kubernetes clients for operations not supported by controller-runtime.
	restCfg := ctrl.GetConfigOrDie()
	k8sClient, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		setupLog.Error(err, "Failed to create Kubernetes client")
		os.Exit(1)
	}
	dynamicClient, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		setupLog.Error(err, "Failed to create dynamic client")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()

	// Detect the cloud provider from node labels.
	cloudProvider := security.DetectCloudProvider(ctx, mgr.GetClient())
	setupLog.Info("Detected cloud provider", "provider", cloudProvider)

	// LLM request timeout (per provider chat/completion call). Defaults to 1h; set
	// LLM_REQUEST_TIMEOUT (e.g. "5m", "30s", "2h") to override via the chart value
	// modelRouter.llmRequestTimeout.
	llmReqTimeout := parseLLMRequestTimeout()
	setupLog.Info("LLM request timeout", "timeout", llmReqTimeout)

	// Context compaction (#47): the conversation is compacted when it reaches
	// CONTEXT_COMPACTION_THRESHOLD of the window (default 0.8), by a model-written
	// summary or, with CONTEXT_COMPACTION_MODE=truncate, by dropping the oldest
	// messages down to CONTEXT_COMPACTION_RATIO. Chart values
	// modelRouter.contextCompaction{Threshold,Mode,Ratio}.
	compactionThreshold := parseFractionEnv("CONTEXT_COMPACTION_THRESHOLD")
	compactionMode := strings.TrimSpace(os.Getenv("CONTEXT_COMPACTION_MODE"))
	if compactionMode != "" && compactionMode != "summary" && compactionMode != "truncate" {
		setupLog.Info("WARNING: unrecognized CONTEXT_COMPACTION_MODE; routers will use summary",
			"mode", compactionMode, "valid", "summary|truncate")
	}
	compactionRatio := parseFractionEnv("CONTEXT_COMPACTION_RATIO")
	if compactionRatio > 0 && compactionMode != "truncate" {
		setupLog.Info("CONTEXT_COMPACTION_RATIO is deprecated: summary compaction has no fixed target, "+
			"so it only applies to CONTEXT_COMPACTION_MODE=truncate and the summary fallback (#47)", "ratio", compactionRatio)
	}
	setupLog.Info("Context compaction", "threshold", compactionThreshold, "mode", compactionMode, "ratio", compactionRatio)

	modelRouterImage := os.Getenv("MODEL_ROUTER_IMAGE")
	if modelRouterImage == "" {
		modelRouterImage = "ghcr.io/heddles/agent-orca/model-router:latest"
	}

	// Connect to the state store so the controller can read spend data
	// persisted by model-router sidecars (survives pod crashes).
	stateConfig := state.Config{
		Backend:  os.Getenv("STATE_BACKEND"),
		RedisURL: os.Getenv("REDIS_URL"),
	}
	var stateStore state.Store
	if stateConfig.Backend != "" {
		const maxWait = 60 * time.Second
		const backoff = 3 * time.Second
		deadline := time.Now().Add(maxWait)
		for {
			stateStore, err = state.NewStoreFromConfig(stateConfig)
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				setupLog.Error(err, "Failed to initialize state store after retries, cost tracking and streaming disabled")
				stateStore = nil
				break
			}
			setupLog.Info("State store not ready, retrying...", "error", err.Error(), "backoff", backoff)
			time.Sleep(backoff)
		}
	} else {
		stateStore, err = state.NewStoreFromConfig(stateConfig)
		if err != nil {
			setupLog.Error(err, "Failed to initialize state store")
			stateStore = nil
		}
	}
	if stateConfig.Backend == "" {
		setupLog.Info("WARNING: No state backend configured (STATE_BACKEND is unset). " +
			"Redis is strongly recommended: without it, cost tracking will always show $0 " +
			"and conversation history will be lost if an agent pod crashes. " +
			"Set STATE_BACKEND=redis and REDIS_URL to enable.")
	}

	// Router configs are written to plain ConfigMaps, so the Redis password is
	// moved out of the URL into a per-run/per-deployment Secret that only the
	// model-router mounts (issue #107).
	routerStateConfig, redisPassword, err := stateConfig.WithPasswordFile(podbuilder.RedisPasswordFile)
	if err != nil {
		setupLog.Error(err, "Invalid REDIS_URL")
		os.Exit(1)
	}

	// ── PostgreSQL archival store (optional) ─────────────────────────────────
	// When POSTGRES_DSN is set, completed AgentRuns are snapshotted to PostgreSQL
	// so the UI can display historical runs beyond the K8s CRD retention window.
	// The CloudNativePG cluster takes a few seconds to bootstrap after install,
	// so we retry the connection for up to 120s (mirrors the Redis retry pattern).
	var pgStore *postgresql.Store
	if pgDsn := os.Getenv("POSTGRES_DSN"); pgDsn != "" {
		const maxWait = 120 * time.Second
		const backoff = 5 * time.Second
		deadline := time.Now().Add(maxWait)
		for {
			pgStore, err = postgresql.NewStore(pgDsn)
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				setupLog.Error(err, "Failed to connect to PostgreSQL after retries, run archival disabled")
				pgStore = nil
				break
			}
			setupLog.Info("PostgreSQL not ready yet, retrying...", "error", err.Error(), "backoff", backoff)
			time.Sleep(backoff)
		}
	} else {
		setupLog.Info("PostgreSQL archival disabled (POSTGRES_DSN not set)")
	}
	if pgStore != nil {
		ctx := context.Background()
		if err := pgStore.ApplyMigration(ctx); err != nil {
			setupLog.Error(err, "Failed to apply PostgreSQL migration")
		}
	}
	// The tenant registry lives in Postgres. Without it tenant IDs are still
	// assigned and mirrored to TenantConfig status, but nothing records their
	// lifecycle, so a deleted tenant's sessions are only stopped by the cache.
	// Assign the interface only when there is a store: a nil *Store in an
	// interface would not compare equal to nil.
	var tenantRegistry tenancy.Registry
	if pgStore != nil {
		tenantRegistry = pgStore
	} else {
		setupLog.Info("WARNING: tenant registry disabled (POSTGRES_DSN not set); " +
			"tenant IDs are not tracked across TenantConfig deletion")
	}

	// ── Alerting subsystem (optional) ────────────────────────────────────────
	// Configured via ALERT_WEBHOOK_URL (comma-separated) and ALERT_HMAC_SECRET.
	var alertManager *apiserver.AlertManager
	if amWebhookURL := os.Getenv("ALERT_WEBHOOK_URL"); amWebhookURL != "" && stateStore != nil {
		webhooks := parseAlertWebhooks(amWebhookURL, os.Getenv("ALERT_HMAC_SECRET"))
		alertManager = apiserver.NewAlertManager(stateStore, webhooks)
	}

	if err := (&controller.ModelProviderReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "ModelProvider")
		os.Exit(1)
	}
	if err := (&controller.ModelSelectorReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "ModelSelector")
		os.Exit(1)
	}
	benchmarks := &controller.RoutingBenchmarkReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}
	if pgStore != nil {
		benchmarks.Results = pgStore.AsPlatform()
	}
	if err := benchmarks.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "RoutingBenchmark")
		os.Exit(1)
	}
	evaluation := &controller.RoutingEvaluationReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Disabled: !routingRecommendations,
	}
	if pgStore != nil {
		evaluation.Source = pgStore.AsPlatform()
	}
	if err := evaluation.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "RoutingEvaluation")
		os.Exit(1)
	}
	if err := (&controller.ToolReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "Tool")
		os.Exit(1)
	}
	if err := (&controller.MCPServerReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "MCPServer")
		os.Exit(1)
	}
	if err := (&controller.AgentReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		CloudProvider: cloudProvider,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "Agent")
		os.Exit(1)
	}
	// OpenShell sandboxes are optional: without a gateway address, Agents with
	// spec.runtime.sandbox fail their runs with a clear reason.
	var sandboxes openshell.Gateway
	sandboxPlacement, err := openshell.PlacementFromEnv()
	if err != nil {
		setupLog.Error(err, "Invalid OpenShell sandbox placement")
		os.Exit(1)
	}
	if osCfg, ok := openshell.ConfigFromEnv(); ok {
		gw, err := openshell.Connect(osCfg, func(ctx context.Context, namespace, pod, container string) (string, error) {
			raw, err := k8sClient.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{Container: container}).DoRaw(ctx)
			return string(raw), err
		})
		if err != nil {
			setupLog.Error(err, "Failed to configure the OpenShell gateway client")
			os.Exit(1)
		}
		sandboxes = gw
		setupLog.Info("OpenShell sandboxes enabled", "gateway", osCfg.Address,
			"workspace", osCfg.Workspace, "sandboxNamespace", osCfg.SandboxNamespace)
	}
	// The GitHub App gives sandboxed agents short-lived git credentials
	// (spec.runtime.sandbox.github); without it such runs fail with a clear reason.
	// Egress approvals need the PostgreSQL store; without it they stay off.
	var approvals controller.ApprovalStore
	if pgStore != nil {
		approvals = pgStore.AsPlatform()
	}
	var approvalNotifier controller.ApprovalNotifier
	var completionNotifier controller.TextNotifier
	if u := os.Getenv("APPROVALS_NOTIFY_WEBHOOK_URL"); u != "" {
		webhookNotifier := &controller.WebhookApprovalNotifier{URL: u, UIURL: os.Getenv("APPROVALS_UI_URL")}
		approvalNotifier, completionNotifier = webhookNotifier, webhookNotifier
	}
	// A recommendation is applied only after an approver agrees, so applying needs
	// the same PostgreSQL-backed approvals as the other approval flows.
	if err := (&controller.RoutingRecommendationReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
		Approvals: approvals, ApprovalNotifier: approvalNotifier,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "RoutingRecommendation")
		os.Exit(1)
	}
	// Runs on a clock. Leader election (on in the chart) keeps the work to one
	// replica; the run name derived from each tick is what actually prevents a
	// double start if two ever run.
	var scheduleAudit controller.ScheduleAuditor
	if pgStore != nil {
		scheduleAudit = pgStore.AsPlatform()
	}
	if err := (&controller.AgentScheduleReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
		StateStore:             stateStore,
		Audit:                  scheduleAudit,
		MaxConsecutiveFailures: scheduleMaxFailures,
		// Result notifications (spec.result.notify). The link in a message uses
		// the UI address the approval messages use.
		Notifier: &notify.Sender{},
		UIURL:    os.Getenv("APPROVALS_UI_URL"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "AgentSchedule")
		os.Exit(1)
	}
	var gitHubApp controller.GitHubTokenMinter
	if dir := os.Getenv("GITHUB_APP_DIR"); dir != "" {
		app, err := githubapp.FromDir(dir, os.Getenv("GITHUB_API_URL"))
		if err != nil {
			setupLog.Error(err, "Failed to load the GitHub App")
			os.Exit(1)
		}
		gitHubApp = app
		setupLog.Info("GitHub App enabled for sandbox git access", "appID", app.AppID, "installationID", app.InstallationID)
	}
	if err := (&controller.AgentRunReconciler{
		Client:                     mgr.GetClient(),
		Scheme:                     mgr.GetScheme(),
		K8s:                        k8sClient,
		Dynamic:                    dynamicClient,
		ModelRouterImage:           modelRouterImage,
		StateConfig:                routerStateConfig,
		RedisPassword:              redisPassword,
		StateStore:                 stateStore,
		CloudProvider:              cloudProvider,
		PostgresStore:              pgStore.AsPlatform(),
		TokenReviewerClusterRole:   os.Getenv("TOKEN_REVIEWER_CLUSTER_ROLE"),
		OperatorAPIURL:             os.Getenv("OPERATOR_API_URL"),
		LLMRequestTimeout:          llmReqTimeout,
		ContextCompactionRatio:     compactionRatio,
		ContextCompactionThreshold: compactionThreshold,
		ContextCompactionMode:      compactionMode,
		HindsightURL:               os.Getenv("HINDSIGHT_URL"),
		HindsightAPIKey:            os.Getenv("HINDSIGHT_API_KEY"),
		OperatorNamespace:          os.Getenv("POD_NAMESPACE"),
		Sandboxes:                  sandboxes,
		SandboxPlacement:           sandboxPlacement,
		GitHubApp:                  gitHubApp,
		Approvals:                  approvals,
		ApprovalNotifier:           approvalNotifier,
		ClusterDomain:              os.Getenv("CLUSTER_DOMAIN"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "AgentRun")
		os.Exit(1)
	}
	if err := (&controller.AgentDeploymentReconciler{
		Client:                     mgr.GetClient(),
		Scheme:                     mgr.GetScheme(),
		K8s:                        k8sClient,
		ModelRouterImage:           modelRouterImage,
		StateConfig:                routerStateConfig,
		RedisPassword:              redisPassword,
		CloudProvider:              cloudProvider,
		TokenReviewerClusterRole:   os.Getenv("TOKEN_REVIEWER_CLUSTER_ROLE"),
		OperatorAPIURL:             os.Getenv("OPERATOR_API_URL"),
		LLMRequestTimeout:          llmReqTimeout,
		ContextCompactionRatio:     compactionRatio,
		ContextCompactionThreshold: compactionThreshold,
		ContextCompactionMode:      compactionMode,
		HindsightURL:               os.Getenv("HINDSIGHT_URL"),
		HindsightAPIKey:            os.Getenv("HINDSIGHT_API_KEY"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "AgentDeployment")
		os.Exit(1)
	}
	// Register validating webhooks for CRD admission control.
	allowedRegistries := strings.Split(os.Getenv("ALLOWED_REGISTRIES"), ",")
	if len(allowedRegistries) == 1 && allowedRegistries[0] == "" {
		allowedRegistries = nil // empty env var → no restriction
		setupLog.Info("WARNING: ALLOWED_REGISTRIES is not set — any container image may be used for agents and tools. " +
			"Set ALLOWED_REGISTRIES to restrict images to trusted registries in production.")
	}
	if err := agentwebhook.SetupAgentRunWebhook(mgr, allowedRegistries); err != nil {
		setupLog.Error(err, "Failed to set up AgentRun webhook")
		os.Exit(1)
	}
	if err := agentwebhook.SetupAgentWebhook(mgr, allowedRegistries); err != nil {
		setupLog.Error(err, "Failed to set up Agent webhook")
		os.Exit(1)
	}
	if err := agentwebhook.SetupToolWebhook(mgr, allowedRegistries); err != nil {
		setupLog.Error(err, "Failed to set up Tool webhook")
		os.Exit(1)
	}
	if err := agentwebhook.SetupAgentScheduleWebhook(mgr, scheduleMinInterval); err != nil {
		setupLog.Error(err, "Failed to set up AgentSchedule webhook")
		os.Exit(1)
	}
	if err := agentwebhook.SetupSandboxClassWebhook(mgr); err != nil {
		setupLog.Error(err, "Failed to set up SandboxClass webhook")
		os.Exit(1)
	}
	var tenantAdminGroups []string
	if g := strings.TrimSpace(os.Getenv("TENANT_ADMIN_GROUP")); g != "" {
		tenantAdminGroups = []string{g}
	} else {
		setupLog.Info("TENANT_ADMIN_GROUP is not set: nobody may restore a deleted tenant ID")
	}
	if err := agentwebhook.SetupTenantConfigWebhook(mgr, tenantRegistry, tenantAdminGroups); err != nil {
		setupLog.Error(err, "Failed to set up TenantConfig webhook")
		os.Exit(1)
	}
	// Tenant label and reference rules. In audit mode (the default) violations
	// are logged, counted and allowed; TENANT_ISOLATION_MODE=enforce denies them.
	if err := tenancy.SetModeFromEnv(); err != nil {
		setupLog.Error(err, "Invalid "+tenancy.ModeEnvVar+"; using audit")
	}
	setupLog.Info("Tenant isolation", "mode", string(tenancy.CurrentMode()))
	tenancy.SetPlatformNamespacesFromEnv()
	if err := agentwebhook.SetupTenantIsolationWebhooks(mgr, tenantAdminGroups, os.Getenv("POD_NAMESPACE")); err != nil {
		setupLog.Error(err, "Failed to set up tenant isolation webhooks")
		os.Exit(1)
	}
	// Ownership: one writer per object, the UI or Git. Its failurePolicy is Fail
	// (chart and config/webhook), independent of the other webhooks.
	ownershipDetect := tenancy.DetectOptionsFromEnv()
	if err := agentwebhook.SetupOwnershipWebhook(mgr, agentwebhook.OwnershipConfig{
		Writers:                tenancy.WritersFromEnv(),
		Detect:                 ownershipDetect,
		OperatorNamespace:      os.Getenv("POD_NAMESPACE"),
		OperatorServiceAccount: os.Getenv("POD_SERVICE_ACCOUNT"),
	}); err != nil {
		setupLog.Error(err, "Failed to set up ownership webhook")
		os.Exit(1)
	}
	if err := controller.SetupHandoffControllers(mgr, ownershipDetect); err != nil {
		setupLog.Error(err, "Failed to set up handoff controllers")
		os.Exit(1)
	}
	// Label objects that predate tenant IDs once the cache is up.
	if err := mgr.Add(&controller.TenantBackfill{
		Client: controller.NewBackfillClient(mgr.GetClient(), mgr.GetAPIReader()),
	}); err != nil {
		setupLog.Error(err, "Failed to add tenant label backfill")
		os.Exit(1)
	}
	// Convert the objects the old UI API created (app.kubernetes.io/managed-by
	// label, no controller owner) to the agentorca.io/owner: ui annotation.
	if err := mgr.Add(&controller.UIOwnerMigration{
		Client: controller.NewBackfillClient(mgr.GetClient(), mgr.GetAPIReader()),
	}); err != nil {
		setupLog.Error(err, "Failed to add UI ownership migration")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	if err := (&controller.AgentWorkflowReconciler{
		Client:             mgr.GetClient(),
		Scheme:             mgr.GetScheme(),
		K8s:                k8sClient,
		Approvals:          approvals,
		ApprovalNotifier:   approvalNotifier,
		CompletionNotifier: completionNotifier,
		AllowedRegistries:  allowedRegistries,
		Sandboxes:          sandboxes,
		SandboxPlacement:   sandboxPlacement,
		GitHubApp:          gitHubApp,
		ClusterDomain:      os.Getenv("CLUSTER_DOMAIN"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "AgentWorkflow")
		os.Exit(1)
	}

	if err := (&controller.KnowledgeBaseReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		MCPIngesterImage: os.Getenv("MCP_INGESTER_IMAGE"),
		QdrantImage:      os.Getenv("QDRANT_IMAGE"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "KnowledgeBase")
		os.Exit(1)
	}

	// Resolve internal API TLS cert/key paths (empty means plain HTTP).
	var internalAPICert, internalAPIKey string
	if internalAPICertPath != "" {
		internalAPICert = filepath.Join(internalAPICertPath, internalAPICertName)
		internalAPIKey = filepath.Join(internalAPICertPath, internalAPICertKey)
		setupLog.Info("Internal API TLS enabled", "cert", internalAPICert, "key", internalAPIKey)
	}

	// Start the internal API server (port 8082) for agent-to-agent communication.
	internalAPI := apiserver.New(k8sClient, mgr.GetClient())
	// Per-user MCP connections (docs/mcp-oauth.md): the service behind the UI
	// endpoints and the router-facing token endpoint. nil leaves both off.
	connService := newConnectionService(pgStore)
	if connService != nil {
		internalAPI.WithMCPTokens(connService)
	}
	// Tool-call approvals (Tool.spec.sideEffect) need the PostgreSQL store; without
	// it routers asking for one get 503 and the gated tool call is refused.
	if pgStore != nil {
		var notifier apiserver.ApprovalNotifier
		if approvalNotifier != nil {
			notifier = approvalNotifier
		}
		internalAPI.WithApprovals(pgStore.AsPlatform(), notifier)
		// Routing telemetry: one row per LLM call, posted by each run's router.
		internalAPI.WithRoutingStore(pgStore.AsPlatform())
		internalAPI.WithShadowStore(pgStore.AsPlatform())
		// Rows archived before tenant IDs existed are attributed by the namespace
		// binding of their TenantConfig (migration 0009 already used the registry).
		// Unattributable rows are logged and stay visible to the platform only.
		if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
			var tcs agentorcav1alpha1.TenantConfigList
			if err := mgr.GetClient().List(ctx, &tcs); err != nil {
				setupLog.Error(err, "Tenant ID backfill skipped: cannot list TenantConfigs")
				return nil
			}
			owner := map[string]string{}
			for i := range tcs.Items {
				id := tcs.Items[i].Status.TenantID
				for _, ns := range tcs.Items[i].Spec.AllowedNamespaces {
					if prev, seen := owner[ns]; seen && prev != id {
						id = "" // shared namespace: no single owner
					}
					owner[ns] = id
				}
			}
			resolve := func(ns string) string { return owner[ns] }
			if _, err := pgStore.BackfillTenantIDs(postgresql.WithPlatform(ctx), resolve); err != nil {
				setupLog.Error(err, "Tenant ID backfill failed")
			}
			return nil
		})); err != nil {
			setupLog.Error(err, "Unable to schedule the tenant ID backfill")
			os.Exit(1)
		}
		if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
			return pgStore.AsPlatform().RunRoutingEventsRetention(
				ctx, routingEventsRetention, postgresql.RoutingEventsRetentionInterval)
		})); err != nil {
			setupLog.Error(err, "Unable to schedule routing telemetry retention")
			os.Exit(1)
		}
	}
	if pgStore != nil {
		if days := archivedRunsRetentionDaysFromEnv(); days > 0 {
			retention := time.Duration(days) * 24 * time.Hour
			if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
				return pgStore.AsPlatform().RunArchivedRunsRetention(ctx, retention, postgresql.ArchivedRunsRetentionInterval)
			})); err != nil {
				setupLog.Error(err, "Unable to schedule archived-run retention")
				os.Exit(1)
			}
			setupLog.Info("Archived-run retention on: output and trace of older runs are emptied", "days", days)
		}
	}
	// Filesystem relay (issue #34): carries ACP fs/* requests from agent pods to
	// the workstation-side bridge. Requires the redis-backed shared state; with
	// no relay both the internal and ACP fs endpoints fail fast with 501.
	fsRelay, fsRelayErr := acpfs.New(acpfs.StateConfig{Backend: stateConfig.Backend, RedisURL: stateConfig.RedisURL}) //nolint:lll
	if fsRelayErr != nil {
		setupLog.Error(fsRelayErr, "Filesystem relay disabled (failed to initialize)")
	} else if fsRelay != nil {
		setupLog.Info("Filesystem relay enabled", "backend", stateConfig.Backend)
	}
	if fsRelay != nil {
		internalAPI.SetFilesystemRelay(fsRelay)
	}
	// sandbox_exec (Agent.spec.shell) needs the OpenShell gateway; without one the
	// internal API answers 501 and the tool reports that to the agent.
	if sandboxes != nil {
		internalAPI.SetShellSandboxes(&controller.ShellSandboxService{
			Client:            mgr.GetClient(),
			Sandboxes:         sandboxes,
			SandboxPlacement:  sandboxPlacement,
			AllowedRegistries: allowedRegistries,
			GitHubApp:         gitHubApp,
		})
	}
	go func() {
		srv := newHTTPServer(":8082", internalAPI.Handler())
		setupLog.Info("Starting internal API server", "addr", srv.Addr, "tls", internalAPICert != "")
		if err := listenAndServeOptionalTLS(srv, internalAPICert, internalAPIKey); err != nil && err != http.ErrServerClosed {
			setupLog.Error(err, "Internal API server failed")
		}
	}()

	// ── External auth (shared by UI API and External API servers) ─────────────
	// Created before the UI API server so OIDC tenant JWT validation can be
	// used as a fallback to K8s SA tokens on port 8083.
	externalAuth, err := apiserver.NewExternalAuth(k8sClient, mgr.GetClient())
	if err != nil {
		setupLog.Error(err, "Failed to create external auth")
		os.Exit(1)
	}
	externalAuth.SetTenantRegistry(tenantRegistry)
	// The role of callers with no user behind them (service accounts, client
	// credentials). MACHINE_DEFAULT_ROLE, chart value operator.machineDefaultRole.
	if v := strings.TrimSpace(os.Getenv("MACHINE_DEFAULT_ROLE")); v != "" {
		if rerr := externalAuth.SetMachineDefaultRole(v); rerr != nil {
			setupLog.Error(rerr, "Invalid MACHINE_DEFAULT_ROLE")
			os.Exit(1)
		}
	}
	// Tokens minted before tenant IDs existed carry no tid. Map them by name for
	// one release; ALLOW_LEGACY_TENANT_TOKENS=false turns that off.
	if v, perr := strconv.ParseBool(os.Getenv("ALLOW_LEGACY_TENANT_TOKENS")); perr == nil {
		externalAuth.SetAllowLegacyTokens(v)
	}
	// Initial tenant cache load.
	if refreshErr := externalAuth.RefreshTenants(ctx); refreshErr != nil {
		setupLog.Info("No TenantConfigs found (will refresh on reconcile)", "err", refreshErr)
	}

	if err := (&controller.TenantConfigReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		Refresher:         externalAuth,
		Registry:          tenantRegistry,
		OperatorNamespace: os.Getenv("POD_NAMESPACE"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "TenantConfig")
		os.Exit(1)
	}

	// ── OIDC interactive login (browser "login via OIDC") ────────────────────
	// Tenant-driven: no operator-wide IdP client. The handler builds the OIDC
	// provider per TenantConfig at /oauth/login|/oauth/callback using the tenant
	// own client_secret (read from its first allowed namespace).
	var uiOIDC *apiserver.OIDCLoginHandler
	if oidcLoginEnabled {
		uiOIDC = apiserver.NewOIDCLoginHandler(externalAuth,
			apiserver.WithOIDCCookieSecure(oidcCookieSecure),
			apiserver.WithOIDCRedirectURI("/"))
	}

	// Start the UI API server (port 8083) for the React frontend.
	uiAPI := apiserver.NewUIServer(k8sClient, mgr.GetClient(), stateConfig.Backend != "", stateStore, uiAuthEnabled, externalAuth, pgStore, alertManager) //nolint:lll
	uiAPI.SetOwnership(ownershipDetect, tenancy.HandoffTTLFromEnv())
	uiAPI.SetScheduleMinInterval(scheduleMinInterval)
	uiAPI.SetRevisionsKeep(apiserver.RevisionsKeepFromEnv())
	evalsCfg := evalsConfigFromEnv()
	uiAPI.SetEvals(evalsCfg)
	uiAPI.SetEvalExperimentLimits(evalExperimentLimitsFromEnv())
	uiAPI.SetBuilder(builderConfigFromEnv())
	// The evaluation experiment runner needs PostgreSQL. It is a manager runnable
	// that wants leader election (the default), so one operator runs it at a time.
	if pgStore != nil && !evalsCfg.DisableExperiments {
		var limiter evalrunner.Limiter
		if rl := externalAuth.RateLimiter(); rl != nil {
			limiter = rl
		}
		if err := mgr.Add(evalrunner.New(pgStore.AsPlatform(), mgr.GetClient(), evalrunner.Config{
			Interval: evalRunnerIntervalFromEnv(), Limiter: limiter,
			Judges:              judge.KubeResolver{Reader: mgr.GetClient()},
			ResultRetentionDays: evalResultRetentionDaysFromEnv(),
		})); err != nil {
			setupLog.Error(err, "Unable to add the evaluation experiment runner")
			os.Exit(1)
		}
	}
	if uiOIDC != nil {
		uiAPI.SetOIDCLogin(uiOIDC)
	}
	if connService != nil {
		uiAPI.SetConnections(connService, pgStore, oidcCookieSecure)
		if err := addConnectionLifecycle(mgr, connService, pgStore.AsPlatform()); err != nil {
			setupLog.Error(err, "Unable to schedule the MCP connection lifecycle jobs")
			os.Exit(1)
		}
	}
	go func() {
		srv := newHTTPServer(":8083", uiAPI.Handler())
		setupLog.Info("Starting UI API server", "addr", srv.Addr, "tls", internalAPICert != "")
		if err := listenAndServeOptionalTLS(srv, internalAPICert, internalAPIKey); err != nil && err != http.ErrServerClosed {
			setupLog.Error(err, "UI API server failed")
		}
	}()

	// Start the external API server (port 8084) for enterprise integrations.
	externalAPI := apiserver.NewExternalAPIServer(k8sClient, mgr.GetClient(), externalAuth, stateConfig.Backend != "", stateStore) //nolint:lll
	if pgStore != nil {
		externalAPI.WithFeedbackStore(pgStore)
	}

	go func() {
		srv := newHTTPServer(":8084", externalAPI.Handler())
		setupLog.Info("Starting external API server", "addr", srv.Addr, "tls", internalAPICert != "")
		if err := listenAndServeOptionalTLS(srv, internalAPICert, internalAPIKey); err != nil && err != http.ErrServerClosed {
			setupLog.Error(err, "External API server failed")
		}
	}()

	// Bootstrap the first admin SA if requested. This runs once at startup;
	// on subsequent restarts the SA already exists and the token is not re-issued.
	if adminBootstrapToken != "" {
		if err := bootstrapAdminSA(ctx, k8sClient); err != nil {
			setupLog.Error(err, "Failed to bootstrap admin SA")
			os.Exit(1)
		}
	}

	// Start the ACP API server (port 8000) for ACP-compatible clients.
	acpAPI := apiserver.NewACPServer(k8sClient, mgr.GetClient(), externalAuth, stateStore)
	if fsRelay != nil {
		acpAPI.SetFilesystemRelay(fsRelay)
	}
	go func() {
		srv := newHTTPServer(":8000", acpAPI.Handler())
		setupLog.Info("Starting ACP API server", "addr", srv.Addr, "tls", internalAPICert != "")
		if err := listenAndServeOptionalTLS(srv, internalAPICert, internalAPIKey); err != nil && err != http.ErrServerClosed {
			setupLog.Error(err, "ACP API server failed")
		}
	}()

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}

// listenAndServeOptionalTLS starts the server with TLS if certFile and keyFile are
// non-empty, otherwise falls back to plain HTTP.
func listenAndServeOptionalTLS(srv *http.Server, certFile, keyFile string) error {
	if certFile != "" && keyFile != "" {
		return srv.ListenAndServeTLS(certFile, keyFile)
	}
	return srv.ListenAndServe()
}

// newHTTPServer creates an http.Server with sensible timeouts. Without
// ReadTimeout/WriteTimeout, a handler that blocks indefinitely (e.g. a slow
// K8s API call or unreachable OIDC provider during auth) holds the connection
// open until the ingress/proxy gives up — surfacing as a 504 Gateway
// Time-out with no server-side error. The 120 s write timeout accommodates
// SSE token streams, which can be long-lived between flushes.
func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  120 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
}

// bootstrapAdminSA creates (or ensures) an admin ServiceAccount in the operator
// namespace and prints a long-lived bearer token to stdout. The token is
// issued via the TokenRequest API with a 365-day expiration so it can be stored
// like a kubeconfig credential. On subsequent restarts the SA already exists;
// we still issue a fresh token and print it (idempotent from the caller's
// perspective — the SA label is what gates access, not the token itself).
func bootstrapAdminSA(ctx context.Context, k8s kubernetes.Interface) error {
	ns := os.Getenv("POD_NAMESPACE")
	if ns == "" {
		ns = "agent-orca-system"
	}
	saName := "agentorca-admin"

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: ns,
			Labels:    map[string]string{"agentorca.io/admin": "true"},
		},
	}
	if _, err := k8s.CoreV1().ServiceAccounts(ns).Create(ctx, sa, metav1.CreateOptions{}); err != nil && !k8serrors.IsAlreadyExists(err) { //nolint:lll

		return fmt.Errorf("creating admin SA: %w", err)
	}

	// Ensure the SA has the admin label even if it pre-existed.
	if _, err := k8s.CoreV1().ServiceAccounts(ns).Get(ctx, saName, metav1.GetOptions{}); err != nil {
		return fmt.Errorf("fetching admin SA: %w", err)
	}

	tokenReq, err := k8s.CoreV1().ServiceAccounts(ns).CreateToken(ctx, saName, &authv1.TokenRequest{
		Spec: authv1.TokenRequestSpec{
			ExpirationSeconds: ptrInt64(365 * 24 * 3600), // 365 days
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("issuing admin token: %w", err)
	}

	setupLog.Info("Admin SA bootstrapped — store this token like a kubeconfig credential",
		"serviceAccount", saName, "namespace", ns)
	fmt.Println("=== agent-orca admin bootstrap token ===")
	fmt.Println(tokenReq.Status.Token)
	fmt.Println("=== end bootstrap token (store securely) ===")
	return nil
}

// ptrInt64 returns a pointer to i.
//
//go:fix inline
func ptrInt64(i int64) *int64 { return new(i) }

// parseLLMRequestTimeout reads the LLM_REQUEST_TIMEOUT env var (a Go duration such as
// "1h", "5m", "90s") and returns it. Defaults to 1 hour when unset or invalid, so the
// model-router's outgoing LLM provider calls are not capped by the old 120s hard limit.
func parseLLMRequestTimeout() time.Duration {
	v := strings.TrimSpace(os.Getenv("LLM_REQUEST_TIMEOUT"))
	if v == "" {
		return time.Hour
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return time.Hour
	}
	return d
}

// parseFractionEnv reads env var name as a float in (0, 1). When unset or
// invalid it returns 0, which the router replaces with its default.
func parseFractionEnv(name string) float64 {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return 0
	}
	ratio, err := strconv.ParseFloat(v, 64)
	if err != nil || ratio <= 0 || ratio >= 1.0 {
		return 0
	}
	return ratio
}

// parseAlertWebhooks parses the ALERT_WEBHOOK_URL env var (comma-separated URLs)
// and the ALERT_HMAC_SECRET env var (base64-encoded key) into AlertWebhook structs.
func parseAlertWebhooks(urls, hmacSecretB64 string) []apiserver.AlertWebhook {
	if urls == "" {
		return nil
	}
	key, _ := base64.StdEncoding.DecodeString(hmacSecretB64)
	var out []apiserver.AlertWebhook
	for u := range strings.SplitSeq(urls, ",") {
		u = strings.TrimSpace(u)
		if u != "" {
			out = append(out, apiserver.AlertWebhook{URL: u, HMACKey: key})
		}
	}
	return out
}

// evalsConfigFromEnv reads the limits on evaluation datasets (chart values
// operator.evals.*). An unset or unreadable value keeps the default.
func evalsConfigFromEnv() apiserver.EvalsConfig {
	var c apiserver.EvalsConfig
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("EVALS_MAX_CASES_PER_DATASET"))); err == nil && n > 0 {
		c.MaxCasesPerDataset = n
	}
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("EVALS_MAX_CASE_BYTES"))); err == nil && n > 0 {
		c.MaxCaseBytes = n
	}
	if v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv("EVALS_ALLOW_UNREDACTED_CASES"))); err == nil {
		c.DisallowUnredactedCases = !v
	}
	if v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv("EVALS_ENABLED"))); err == nil {
		c.DisableExperiments = !v
	}
	if v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv("EVALS_REQUIRE_INDEPENDENT_REVIEW"))); err == nil {
		c.RequireIndependentReview = v
	}
	if v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv("EVALS_ALLOW_GATE_OVERRIDE"))); err == nil {
		c.DisallowGateOverride = !v
	}
	return c
}

// evalExperimentLimitsFromEnv reads the limits on experiments (chart values
// operator.evals.*). An unset or unreadable value keeps the default.
func evalExperimentLimitsFromEnv() postgresql.EvalExperimentLimits {
	var l postgresql.EvalExperimentLimits
	env := func(name string) string { return strings.TrimSpace(os.Getenv(name)) }
	if n, err := strconv.Atoi(env("EVALS_MAX_ACTIVE_EXPERIMENTS_PER_TENANT")); err == nil && n > 0 {
		l.MaxActivePerTenant = n
	}
	if n, err := strconv.Atoi(env("EVALS_MAX_CASES_PER_EXPERIMENT")); err == nil && n > 0 {
		l.MaxCasesPerExperiment = n
	}
	if v, err := strconv.ParseFloat(env("EVALS_MAX_BUDGET_PER_EXPERIMENT_USD"), 64); err == nil && v > 0 {
		l.MaxBudgetUSD = v
	}
	return l
}

// builderConfigFromEnv reads the limits of the agent builder's helpers (chart
// values operator.builder.*). An unset or unreadable value keeps the default.
func builderConfigFromEnv() apiserver.BuilderConfig {
	var c apiserver.BuilderConfig
	env := func(name string) string { return strings.TrimSpace(os.Getenv(name)) }
	num := func(name string, into *int) {
		if n, err := strconv.Atoi(env(name)); err == nil && n > 0 {
			*into = n
		}
	}
	usd := func(name string, into *float64) {
		if v, err := strconv.ParseFloat(env(name), 64); err == nil && v > 0 {
			*into = v
		}
	}
	usd("BUILDER_CHECK_DEFAULT_BUDGET_USD", &c.CheckDefaultBudgetUSD)
	usd("BUILDER_CHECK_MAX_BUDGET_USD", &c.CheckMaxBudgetUSD)
	num("BUILDER_CHECK_MAX_CASES", &c.CheckMaxCases)
	num("BUILDER_CHECK_MAX_CONCURRENCY", &c.CheckMaxConcurrency)
	num("BUILDER_CHECK_PER_USER_PER_HOUR", &c.CheckPerUserPerHour)
	return c
}

// evalRunnerIntervalFromEnv is how often the experiment runner polls
// (EVALS_RUNNER_INTERVAL_SECONDS, default 5).
func evalRunnerIntervalFromEnv() time.Duration {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("EVALS_RUNNER_INTERVAL_SECONDS"))); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return 0
}

// evalResultRetentionDaysFromEnv is how long evaluation result outputs and
// grades are kept (EVALS_RESULT_RETENTION_DAYS, default 90; 0 keeps them).
func evalResultRetentionDaysFromEnv() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("EVALS_RESULT_RETENTION_DAYS"))); err == nil && n >= 0 {
		return n
	}
	return 90
}

// archivedRunsRetentionDaysFromEnv is how long an archived run keeps its output
// and execution trace (ARCHIVED_RUNS_RETENTION_DAYS; 0, the default, keeps them).
// The summary row (status, cost, tokens, times) is always kept.
func archivedRunsRetentionDaysFromEnv() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("ARCHIVED_RUNS_RETENTION_DAYS"))); err == nil && n > 0 {
		return n
	}
	return 0
}

// connectionIdleExpiryFromEnv is how long a per-user MCP connection may go unused
// before it is revoked and expired (CONNECTION_IDLE_EXPIRY_DAYS, default 90; 0
// turns the expiry off).
func connectionIdleExpiryFromEnv() time.Duration {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("CONNECTION_IDLE_EXPIRY_DAYS"))); err == nil && n >= 0 {
		return time.Duration(n) * 24 * time.Hour
	}
	return connections.DefaultIdleAfter
}

// addConnectionLifecycle schedules the leader-only jobs of the per-user
// connections (idle expiry, key re-wrapping) and registers their gauge.
func addConnectionLifecycle(mgr manager.Manager, svc *connections.Service, platform *postgresql.Store) error {
	if err := connections.RegisterStatusGauge(platform); err != nil {
		return err
	}
	life := &connections.Lifecycle{
		Store: platform, Svc: svc, IdleAfter: connectionIdleExpiryFromEnv(),
		Resolve: func(ctx context.Context, ns, name string) (connections.ServerRef, error) {
			var srv agentorcav1alpha1.MCPServer
			if err := mgr.GetClient().Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &srv); err != nil {
				return connections.ServerRef{}, err
			}
			o := srv.Spec.Auth
			noRevoke := o != nil && o.OAuth != nil && o.OAuth.RevokeOnDisconnect != nil && !*o.OAuth.RevokeOnDisconnect
			if o == nil || o.OAuth == nil || !o.OAuth.PerUser() || noRevoke {
				return connections.ServerRef{}, errors.New("no provider revocation for this server")
			}
			return connections.ServerRef{Namespace: srv.Namespace, Name: srv.Name, UID: string(srv.UID), URL: srv.Spec.URL,
				AppRef: o.OAuth.AppRef, UserScopes: o.OAuth.UserScopes}, nil
		},
		OnExpire: func(ctx context.Context, c postgresql.MCPConnection, revocation string) {
			_, aerr := platform.AppendAudit(postgresql.WithPlatform(ctx), postgresql.AuditEvent{
				TenantID: c.TenantID, UserID: "system:mcp-connections", Action: postgresql.AuditExpire,
				Kind: postgresql.AuditKindMCPConnection, Namespace: c.ServerNS, Name: c.ServerName,
				Summary: "expired the unused connection of user " + c.UserID + " (provider revocation: " + revocation + ")",
			})
			if aerr != nil {
				setupLog.Error(aerr, "Could not audit an expired connection", "server", c.ServerNS+"/"+c.ServerName)
			}
		},
	}
	setupLog.Info("MCP connection lifecycle jobs scheduled", "idleExpiry", life.IdleAfter.String())
	return mgr.Add(life)
}

// newConnectionService builds the per-user MCP connection service
// (docs/mcp-oauth.md). It needs the PostgreSQL store and a key-encryption key:
// CONNECTION_KEY_DIR is a directory mounted from the Secret named by
// database.connectionKeySecret. Anything missing leaves the feature off: the
// endpoints answer 503 and per-user servers get no token.
func newConnectionService(pgStore *postgresql.Store) *connections.Service {
	dir := os.Getenv("CONNECTION_KEY_DIR")
	if dir == "" {
		setupLog.Info("per-user MCP connections are off: no connection key configured (database.connectionKeySecret)")
		return nil
	}
	if pgStore == nil {
		setupLog.Info("per-user MCP connections are off: they need the PostgreSQL store")
		return nil
	}
	keys, err := connections.LoadKeyringDir(dir)
	if err != nil {
		setupLog.Error(err, "Per-user MCP connections are off: cannot load the connection keys")
		return nil
	}
	redirect := os.Getenv("CONNECTION_REDIRECT_URI")
	if redirect == "" {
		setupLog.Info("per-user MCP connections are off: set operator.connections.redirectURI (the UI callback URL)")
		return nil
	}
	allowInsecure, _ := strconv.ParseBool(os.Getenv("CONNECTION_ALLOW_INSECURE_OAUTH"))
	platform := pgStore.AsPlatform()
	svc := &connections.Service{
		Store:         platform,
		Keys:          keys,
		OAuth:         oauth.New(&http.Client{Timeout: 15 * time.Second}, nil),
		RedirectURI:   redirect,
		AllowInsecure: allowInsecure,
	}
	// A refresh that fails with invalid_grant is audited as the system.
	svc.OnReauth = func(ctx context.Context, c postgresql.MCPConnection) {
		_, aerr := platform.AppendAudit(postgresql.WithPlatform(ctx), postgresql.AuditEvent{
			TenantID: c.TenantID, UserID: "system:mcp-connections", Action: postgresql.AuditReauth,
			Kind: postgresql.AuditKindMCPConnection, Namespace: c.ServerNS, Name: c.ServerName,
			Summary: "refresh failed with invalid_grant for user " + c.UserID + ": the connection needs to be renewed",
		})
		if aerr != nil {
			setupLog.Error(aerr, "Could not audit a connection that needs renewing", "server", c.ServerNS+"/"+c.ServerName)
		}
	}
	setupLog.Info("Per-user MCP connections enabled", "activeKey", keys.ActiveID(), "redirectURI", redirect)
	return svc
}
