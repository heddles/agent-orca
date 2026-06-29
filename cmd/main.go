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
	"crypto/tls"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	agentorcv1alpha1 "github.com/floppyfish14/agent-orc/api/v1alpha1"
	"github.com/floppyfish14/agent-orc/internal/apiserver"
	"github.com/floppyfish14/agent-orc/internal/controller"
	"github.com/floppyfish14/agent-orc/internal/security"
	"github.com/floppyfish14/agent-orc/internal/state"
	agentwebhook "github.com/floppyfish14/agent-orc/internal/webhook"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(agentorcv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
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
	flag.StringVar(&internalAPICertName, "internal-api-cert-name", "tls.crt", "The name of the internal API certificate file.")
	flag.StringVar(&internalAPICertKey, "internal-api-cert-key", "tls.key", "The name of the internal API key file.")
	var uiAuthEnabled bool
	flag.BoolVar(&uiAuthEnabled, "ui-auth-enabled", false,
		"Require a valid UIProxy Kubernetes SA token (audience 'agentorc/ui') on all UI API "+
			"requests. Set to true in production deployments where the UIProxy pod provides the token. "+
			"Leave false for local development (npm run dev + operator without the proxy).")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

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
		LeaderElectionID:       "6950eef6.agentorc.io",
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

	modelRouterImage := os.Getenv("MODEL_ROUTER_IMAGE")
	if modelRouterImage == "" {
		modelRouterImage = "ghcr.io/agentorc/agent-orc/model-router:latest"
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
	if err := (&controller.AgentRunReconciler{
		Client:                   mgr.GetClient(),
		Scheme:                   mgr.GetScheme(),
		K8s:                      k8sClient,
		Dynamic:                  dynamicClient,
		ModelRouterImage:         modelRouterImage,
		StateConfig:              stateConfig,
		StateStore:               stateStore,
		CloudProvider:            cloudProvider,
		TokenReviewerClusterRole: os.Getenv("TOKEN_REVIEWER_CLUSTER_ROLE"),
		OperatorAPIURL:           os.Getenv("OPERATOR_API_URL"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "AgentRun")
		os.Exit(1)
	}
	if err := (&controller.AgentDeploymentReconciler{
		Client:                   mgr.GetClient(),
		Scheme:                   mgr.GetScheme(),
		K8s:                      k8sClient,
		ModelRouterImage:         modelRouterImage,
		StateConfig:              stateConfig,
		CloudProvider:            cloudProvider,
		TokenReviewerClusterRole: os.Getenv("TOKEN_REVIEWER_CLUSTER_ROLE"),
		OperatorAPIURL:           os.Getenv("OPERATOR_API_URL"),
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
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
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
	go func() {
		srv := &http.Server{Addr: ":8082", Handler: internalAPI.Handler()}
		setupLog.Info("Starting internal API server", "addr", srv.Addr, "tls", internalAPICert != "")
		if err := listenAndServeOptionalTLS(srv, internalAPICert, internalAPIKey); err != nil && err != http.ErrServerClosed {
			setupLog.Error(err, "Internal API server failed")
		}
	}()

	// Start the UI API server (port 8083) for the React frontend.
	uiAPI := apiserver.NewUIServer(k8sClient, mgr.GetClient(), stateConfig.Backend != "", stateStore, uiAuthEnabled)
	go func() {
		srv := &http.Server{Addr: ":8083", Handler: uiAPI.Handler()}
		setupLog.Info("Starting UI API server", "addr", srv.Addr, "tls", internalAPICert != "")
		if err := listenAndServeOptionalTLS(srv, internalAPICert, internalAPIKey); err != nil && err != http.ErrServerClosed {
			setupLog.Error(err, "UI API server failed")
		}
	}()

	// Start the external API server (port 8084) for enterprise integrations.
	externalAuth, err := apiserver.NewExternalAuth(k8sClient, mgr.GetClient())
	if err != nil {
		setupLog.Error(err, "Failed to create external auth")
		os.Exit(1)
	}
	// Initial tenant cache load.
	if refreshErr := externalAuth.RefreshTenants(ctx); refreshErr != nil {
		setupLog.Info("No TenantConfigs found (will refresh on reconcile)", "err", refreshErr)
	}

	if err := (&controller.TenantConfigReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Refresher: externalAuth,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "TenantConfig")
		os.Exit(1)
	}

	externalAPI := apiserver.NewExternalAPIServer(k8sClient, mgr.GetClient(), externalAuth, stateConfig.Backend != "", stateStore)
	go func() {
		srv := &http.Server{Addr: ":8084", Handler: externalAPI.Handler()}
		setupLog.Info("Starting external API server", "addr", srv.Addr, "tls", internalAPICert != "")
		if err := listenAndServeOptionalTLS(srv, internalAPICert, internalAPIKey); err != nil && err != http.ErrServerClosed {
			setupLog.Error(err, "External API server failed")
		}
	}()

	// Start the ACP API server (port 8000) for ACP-compatible clients.
	acpAPI := apiserver.NewACPServer(k8sClient, mgr.GetClient(), externalAuth, stateStore)
	go func() {
		srv := &http.Server{Addr: ":8000", Handler: acpAPI.Handler()}
		setupLog.Info("Starting ACP API server", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
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
