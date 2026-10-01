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

package controller

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/rag"
)

const (
	qdrantPort       = 6334
	qdrantHTTPPort   = 6333
	qdrantNamePrefix = "kb-qdrant-"

	qdrantImageRepo   = "qdrant/qdrant"
	qdrantGoClientMod = "github.com/qdrant/go-client"
)

// defaultQdrantImage derives the Qdrant image tag from the compiled-in
// go-client module version so the two can never silently drift apart.
// Falls back to "latest" only if build info is unavailable (e.g. `go run`
// without module support).
func defaultQdrantImage() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range bi.Deps {
			if dep.Path == qdrantGoClientMod {
				// Module version is e.g. "v1.17.1"; use as image tag directly.
				return qdrantImageRepo + ":" + dep.Version
			}
		}
	}
	return qdrantImageRepo + ":latest"
}

// KnowledgeBaseReconciler reconciles KnowledgeBase objects.
type KnowledgeBaseReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	MCPIngesterImage string
	QdrantImage      string
}

// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=knowledgebases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=knowledgebases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=knowledgebases/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=mcpservers,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=tools,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

func (r *KnowledgeBaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var kb agentorcav1alpha1.KnowledgeBase
	if err := r.Get(ctx, req.NamespacedName, &kb); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Resolve collection name.
	collectionName := kb.Spec.VectorStore.CollectionName
	if collectionName == "" {
		collectionName = kb.Name
	}

	// Snapshot for patch — avoids optimistic concurrency conflicts on status updates
	// that happen across multiple early-return paths in this reconcile.
	statusBase := kb.DeepCopy()

	// Ensure per-KB dedicated Qdrant StatefulSet + Service exist.
	// If a Qdrant version upgrade is in progress, pause reconcile until the
	// new pod is ready before proceeding to collection/ingestion work.
	qdrantURL, upgrade, err := r.ensureQdrant(ctx, kb.Namespace, &kb)
	if err != nil {
		logger.Error(err, "ensuring Qdrant")
		r.patchKBStatus(ctx, &kb, statusBase, fmt.Sprintf("ensuring Qdrant: %s", sanitizeKBError(err.Error())))
		return ctrl.Result{}, err
	}
	if upgrade != nil && upgrade.Requeue {
		// Preserve any upgrade conditions set during ensureQdrant.
		r.patchKBStatus(ctx, &kb, statusBase, upgradeMessage(upgrade))
		return ctrl.Result{RequeueAfter: upgrade.RequeueAfter}, nil
	}

	// Resolve embedding model and discover vector dimension on first use.
	// The embedder is resolved here (not just inside runIngestion) so we can
	// probe the dimension before creating the Qdrant collection.
	embedder, err := r.resolveEmbedder(ctx, &kb)
	if err != nil {
		logger.Error(err, "resolving embedder")
		r.patchKBStatus(ctx, &kb, statusBase, fmt.Sprintf("resolving embedder: %s", sanitizeKBError(err.Error())))
		return ctrl.Result{}, err
	}
	if kb.Status.EmbeddingDimensions == 0 {
		dims, err := embedder.ProbeDimension(ctx)
		if err != nil {
			logger.Error(err, "discovering embedding dimension")
			r.patchKBStatus(ctx, &kb, statusBase, fmt.Sprintf("discovering embedding dimension: %s", sanitizeKBError(err.Error())))
			return ctrl.Result{}, fmt.Errorf("discovering embedding dimension: %w", err)
		}
		logger.Info("discovered embedding dimension", "dimensions", dims)
		kb.Status.EmbeddingDimensions = dims
	}

	// Wait for the Qdrant pod to be Ready before touching collections. ensureQdrant
	// creates the StatefulSet but the pod may still be starting (image pull / PVC
	// bind / /readyz). Calling ListCollections before the pod has endpoints yields
	// "name resolver error: produced zero addresses" and churns the reconcile.
	if !r.qdrantReady(ctx, kb.Namespace, kb.Name) {
		logger.Info("Qdrant not ready yet; requeuing", "namespace", kb.Namespace, "kb", kb.Name)
		r.patchKBStatus(ctx, &kb, statusBase, "Qdrant pod is not ready yet; waiting for startup to complete")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Ensure the Qdrant collection exists with the actual model dimension.
	if err := r.ensureCollection(ctx, qdrantURL, collectionName, uint64(kb.Status.EmbeddingDimensions)); err != nil {
		logger.Error(err, "ensuring Qdrant collection")
		r.patchKBStatus(ctx, &kb, statusBase, fmt.Sprintf("ensuring Qdrant collection: %s", sanitizeKBError(err.Error())))
		return ctrl.Result{}, err
	}

	// Set status fields (written once at the end of Reconcile to avoid conflicts).
	kb.Status.VectorStoreURL = qdrantURL
	kb.Status.CollectionName = collectionName
	kb.Status.Ready = true
	kb.Status.Message = ""
	setCondition(&kb.Status.Conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             "Ready",
		Message:            fmt.Sprintf("KnowledgeBase is ready (collection: %s)", collectionName),
		LastTransitionTime: metav1.Now(),
	})

	// Run source ingestion if configured.
	if kb.Spec.Ingestion != nil {
		if err := r.runIngestion(ctx, &kb, embedder); err != nil {
			logger.Error(err, "running ingestion")
			// Don't fail reconcile — ingestion can retry on next sync.
		}
	}

	// Query Qdrant for the authoritative counts. This is the single source
	// of truth — it reflects all writes from both the controller pipeline
	// (ConfigMap/URL) and MCP ingestion Jobs.
	if stats, err := r.collectionStats(ctx, qdrantURL, collectionName); err != nil {
		logger.Error(err, "querying Qdrant collection stats")
	} else {
		kb.Status.ChunkCount = int(stats.PointCount)
		kb.Status.DocumentCount = stats.DocumentCount
	}

	// Estimate storage usage from PVC capacity and vector data size.
	kb.Status.StorageUsedPercent = r.estimateStoragePercent(&kb, uint64(kb.Status.EmbeddingDimensions))

	// Single status write for all fields set during this reconcile.
	if err := r.Status().Patch(ctx, &kb, client.MergeFrom(statusBase)); err != nil {
		return ctrl.Result{}, err
	}

	// Re-sync if configured.
	if kb.Spec.Ingestion != nil && kb.Spec.Ingestion.SyncIntervalSeconds > 0 {
		return ctrl.Result{RequeueAfter: durationFromSeconds(kb.Spec.Ingestion.SyncIntervalSeconds)}, nil
	}
	return ctrl.Result{}, nil
}

// patchKBStatus writes a failure or waiting status to the KnowledgeBase CR, setting
// the Ready condition and Message field so the UI (and downstream controllers like
// AgentDeploymentReconciler) can surface a helpful explanation. It always marks the
// KB not-ready; every call site reports a blocking error or a pending condition.
// Errors from the status patch are logged and discarded so they don't mask the
// original reconciler error — the original error is what drives requeue behaviour.
func (r *KnowledgeBaseReconciler) patchKBStatus(
	ctx context.Context,
	kb *agentorcav1alpha1.KnowledgeBase,
	base *agentorcav1alpha1.KnowledgeBase,
	message string,
) {
	kb.Status.Ready = false
	kb.Status.Message = message

	setCondition(&kb.Status.Conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             "NotReady",
		Message:            message,
		LastTransitionTime: metav1.Now(),
	})

	if err := r.Status().Patch(ctx, kb, client.MergeFrom(base)); err != nil {
		log.FromContext(ctx).Error(err, "patching KnowledgeBase status", "kb", kb.Name)
	}
}

// upgradeMessage returns a human-readable status message for an in-progress
// Qdrant upgrade step, so the UI can show the user *why* the KB is waiting.
func upgradeMessage(upgrade *upgradeResult) string {
	if upgrade == nil {
		return ""
	}
	return fmt.Sprintf("Qdrant is being upgraded; requeuing in %s", upgrade.RequeueAfter)
}

// upgradeResult signals whether the reconcile loop should pause for an in-progress Qdrant upgrade.
type upgradeResult struct {
	Requeue      bool
	RequeueAfter time.Duration
}

// qdrantReady reports whether this KB's Qdrant pod is Running and Ready (readiness
// probe /readyz passed), i.e. the headless Service has an endpoint gRPC can dial.
// Until the pod is Ready, ListCollections fails with "produced zero addresses".
func (r *KnowledgeBaseReconciler) qdrantReady(ctx context.Context, namespace, kbName string) bool {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(namespace),
		client.MatchingLabels{"agentorca.io/knowledgebase": kbName},
	); err != nil {
		return false
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Name == "qdrant" && cs.Ready {
				return true
			}
		}
	}
	return false
}

// ensureQdrant creates a dedicated per-KB Qdrant StatefulSet and Service if they don't exist.
// For existing StatefulSets, it checks for version upgrades and walks through sequential
// minor versions when autoUpgrade is enabled. Returns the Qdrant gRPC URL and an optional
// upgradeResult indicating the reconcile should pause.
func (r *KnowledgeBaseReconciler) ensureQdrant(ctx context.Context, namespace string, kb *agentorcav1alpha1.KnowledgeBase) (string, *upgradeResult, error) {
	logger := log.FromContext(ctx)

	resourceName := qdrantResourceName(kb.Name)

	storageSize := kb.Spec.VectorStore.StorageSize
	if storageSize == "" {
		storageSize = "10Gi"
	}

	// Per-KB pod selector labels — unique to this KB's Qdrant pod.
	podLabels := map[string]string{
		"app.kubernetes.io/name":       "qdrant",
		"app.kubernetes.io/managed-by": "agentorca",
		"agentorca.io/knowledgebase":   kb.Name,
	}

	grpcURL := fmt.Sprintf("%s.%s.svc.cluster.local:%d", resourceName, namespace, qdrantPort)
	httpAddr := fmt.Sprintf("%s.%s.svc.cluster.local:%d", resourceName, namespace, qdrantHTTPPort)

	// Check if StatefulSet already exists.
	var existing appsv1.StatefulSet
	err := r.Get(ctx, client.ObjectKey{Name: resourceName, Namespace: namespace}, &existing)
	if err == nil {
		// StatefulSet exists — check for version upgrade.
		upgrade, err := r.checkQdrantUpgrade(ctx, kb, &existing, httpAddr)
		if err != nil {
			logger.Error(err, "checking Qdrant upgrade")
			return grpcURL, nil, nil // non-fatal; proceed with current version
		}
		return grpcURL, upgrade, nil
	}
	if !apierrors.IsNotFound(err) {
		return "", nil, err
	}

	logger.Info("creating per-KB Qdrant StatefulSet", "name", resourceName, "namespace", namespace)

	// Create headless Service.
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName,
			Namespace: namespace,
			Labels:    podLabels,
		},
		Spec: corev1.ServiceSpec{
			Selector:  podLabels,
			ClusterIP: "None",
			Ports: []corev1.ServicePort{
				{Name: "grpc", Port: int32(qdrantPort), TargetPort: intstr.FromInt(qdrantPort)},
				{Name: "http", Port: int32(qdrantHTTPPort), TargetPort: intstr.FromInt(qdrantHTTPPort)},
			},
		},
	}
	if err := ctrl.SetControllerReference(kb, svc, r.Scheme); err != nil {
		return "", nil, fmt.Errorf("setting owner ref on Qdrant Service: %w", err)
	}
	if err := r.Create(ctx, svc); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", nil, fmt.Errorf("creating Qdrant Service: %w", err)
	}

	// Create StatefulSet.
	// Defaults: 256Mi/100m request, 512Mi memory limit. These can be LOW for Qdrant
	// at startup on constrained nodes (arm64 kind), so the limit is overridable via
	// spec.vectorStore.resources — the field existed but was previously unwired.
	memRequest := resource.MustParse("512Mi")
	memLimit := resource.MustParse("1Gi")
	cpuRequest := resource.MustParse("100m")
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceMemory: memRequest,
			corev1.ResourceCPU:    cpuRequest,
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: memLimit,
		},
	}
	// A partial override (e.g. only limits) is merged onto the defaults.
	if kb.Spec.VectorStore.Resources != nil {
		if kb.Spec.VectorStore.Resources.Requests != nil {
			resources.Requests = kb.Spec.VectorStore.Resources.Requests
		}
		if kb.Spec.VectorStore.Resources.Limits != nil {
			resources.Limits = kb.Spec.VectorStore.Resources.Limits
		}
	}

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName,
			Namespace: namespace,
			Labels:    podLabels,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: resourceName,
			Replicas:    ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: podLabels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: podLabels,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "qdrant",
							Image: r.qdrantImageOrDefault(),
							Ports: []corev1.ContainerPort{
								{Name: "grpc", ContainerPort: int32(qdrantPort)},
								{Name: "http", ContainerPort: int32(qdrantHTTPPort)},
							},
							Resources: resources,
							VolumeMounts: []corev1.VolumeMount{
								{Name: "qdrant-storage", MountPath: "/qdrant/storage"},
								{Name: "qdrant-snapshots", MountPath: "/qdrant/snapshots"},
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/readyz",
										Port: intstr.FromInt(qdrantHTTPPort),
									},
								},
								InitialDelaySeconds: 5,
								PeriodSeconds:       10,
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name:         "qdrant-snapshots",
							VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
						},
					},
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr.To(true),
						RunAsUser:    ptr.To(int64(1000)),
						FSGroup:      ptr.To(int64(1000)),
					},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "qdrant-storage"},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse(storageSize),
							},
						},
					},
				},
			},
		},
	}
	if err := ctrl.SetControllerReference(kb, sts, r.Scheme); err != nil {
		return "", nil, fmt.Errorf("setting owner ref on Qdrant StatefulSet: %w", err)
	}
	if err := r.Create(ctx, sts); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", nil, fmt.Errorf("creating Qdrant StatefulSet: %w", err)
	}

	return grpcURL, nil, nil
}

const (
	conditionQdrantUpgrading        = "QdrantUpgrading"
	conditionQdrantUpgradeAvailable = "QdrantUpgradeAvailable"
	conditionReady                  = "Ready"
	qdrantUpgradeRequeueInterval    = 15 * time.Second
	qdrantUpgradeCrashLoopTimeout   = 5 * time.Minute
)

// checkQdrantUpgrade probes the running Qdrant version and determines whether
// an upgrade is needed. If autoUpgrade is enabled, it delegates to stepQdrantUpgrade.
func (r *KnowledgeBaseReconciler) checkQdrantUpgrade(
	ctx context.Context,
	kb *agentorcav1alpha1.KnowledgeBase,
	sts *appsv1.StatefulSet,
	httpAddr string,
) (*upgradeResult, error) {
	logger := log.FromContext(ctx)

	// Determine target version from the operator's configured image.
	targetImage := r.qdrantImageOrDefault()
	targetTag := imageTag(targetImage)
	target, err := parseQdrantVersion(targetTag)
	if err != nil {
		return nil, fmt.Errorf("parsing target qdrant version from image %q: %w", targetImage, err)
	}
	kb.Status.QdrantTargetVersion = target.String()

	// If we're in WaitingForReady or Failed state, handle that without probing version.
	if kb.Status.QdrantUpgradeState == "WaitingForReady" || kb.Status.QdrantUpgradeState == "Failed" { //nolint:goconst

		return r.stepQdrantUpgrade(ctx, kb, sts, qdrantVersion{}, target)
	}

	// Probe current version from the running Qdrant instance.
	currentStr, err := rag.QdrantServerVersion(ctx, httpAddr)
	if err != nil {
		// Pod may not be ready yet — don't block reconcile.
		logger.V(1).Info("could not probe Qdrant version", "error", err)
		return nil, nil
	}
	current, err := parseQdrantVersion(currentStr)
	if err != nil {
		return nil, fmt.Errorf("parsing current qdrant version %q: %w", currentStr, err)
	}
	kb.Status.QdrantVersion = current.String()

	if !needsUpgrade(current, target) {
		// Clear any stale upgrade conditions.
		setCondition(&kb.Status.Conditions, metav1.Condition{
			Type:               conditionQdrantUpgrading,
			Status:             metav1.ConditionFalse,
			Reason:             "UpToDate",
			Message:            fmt.Sprintf("Qdrant is at target version %s", target),
			LastTransitionTime: metav1.Now(),
		})
		setCondition(&kb.Status.Conditions, metav1.Condition{
			Type:               conditionQdrantUpgradeAvailable,
			Status:             metav1.ConditionFalse,
			Reason:             "UpToDate",
			Message:            fmt.Sprintf("Qdrant is at target version %s", target),
			LastTransitionTime: metav1.Now(),
		})
		kb.Status.QdrantUpgradeState = ""
		return nil, nil
	}

	// Upgrade available. Check if autoUpgrade is enabled.
	autoUpgrade := kb.Spec.VectorStore.AutoUpgrade == nil || *kb.Spec.VectorStore.AutoUpgrade
	if !autoUpgrade {
		setCondition(&kb.Status.Conditions, metav1.Condition{
			Type:               conditionQdrantUpgradeAvailable,
			Status:             metav1.ConditionTrue,
			Reason:             "UpgradeHeld",
			Message:            fmt.Sprintf("Qdrant %s → %s available; autoUpgrade is disabled", current, target),
			LastTransitionTime: metav1.Now(),
		})
		return nil, nil
	}

	return r.stepQdrantUpgrade(ctx, kb, sts, current, target)
}

// stepQdrantUpgrade implements the sequential upgrade state machine.
// It advances one minor version per call, creating a snapshot before each step.
func (r *KnowledgeBaseReconciler) stepQdrantUpgrade(
	ctx context.Context,
	kb *agentorcav1alpha1.KnowledgeBase,
	sts *appsv1.StatefulSet,
	current, target qdrantVersion,
) (*upgradeResult, error) {
	logger := log.FromContext(ctx)

	switch kb.Status.QdrantUpgradeState {
	case "Failed":
		// User must toggle autoUpgrade false→true to retry.
		return nil, nil

	case "WaitingForReady":
		// Check if the pod is ready after the image update.
		if sts.Status.ReadyReplicas >= 1 {
			logger.Info("Qdrant upgrade step complete, pod is ready")
			kb.Status.QdrantUpgradeState = ""
			// Requeue immediately to probe the new version and start the next step if needed.
			return &upgradeResult{Requeue: true, RequeueAfter: 0}, nil
		}

		// Check for CrashLoopBackOff — if the pod has been failing for too long, roll back.
		if r.qdrantPodCrashLooping(ctx, sts) {
			logger.Error(nil, "Qdrant pod in CrashLoopBackOff after upgrade, rolling back")
			// Revert to the previous image (stored in status.QdrantVersion).
			if kb.Status.QdrantVersion != "" {
				prevVersion, err := parseQdrantVersion(kb.Status.QdrantVersion)
				if err == nil {
					repo := imageRepo(r.qdrantImageOrDefault())
					if err := r.patchStatefulSetImage(ctx, sts, prevVersion.Image(repo)); err != nil {
						return nil, fmt.Errorf("rolling back Qdrant image: %w", err)
					}
				}
			}
			kb.Status.QdrantUpgradeState = "Failed"
			setCondition(&kb.Status.Conditions, metav1.Condition{
				Type:               conditionQdrantUpgrading,
				Status:             metav1.ConditionFalse,
				Reason:             "UpgradeFailed",
				Message:            "Qdrant pod failed to start after upgrade; rolled back",
				LastTransitionTime: metav1.Now(),
			})
			return &upgradeResult{Requeue: true, RequeueAfter: 0}, nil
		}

		// Still rolling — requeue.
		return &upgradeResult{Requeue: true, RequeueAfter: qdrantUpgradeRequeueInterval}, nil

	default:
		// Idle — start the next upgrade step.
		path, err := qdrantUpgradePath(current, target)
		if err != nil {
			return nil, fmt.Errorf("computing upgrade path: %w", err)
		}
		if len(path) == 0 {
			return nil, nil // nothing to do
		}

		nextVersion := path[0]
		repo := imageRepo(r.qdrantImageOrDefault())
		collectionName := kb.Status.CollectionName
		if collectionName == "" {
			collectionName = kb.Name
		}

		logger.Info("starting Qdrant upgrade step",
			"from", current, "to", nextVersion, "target", target,
			"stepsRemaining", len(path))

		// Snapshot before upgrade.
		httpAddr := fmt.Sprintf("%s.%s.svc.cluster.local:%d",
			qdrantResourceName(kb.Name), kb.Namespace, qdrantHTTPPort)
		snapshotName, err := rag.CreateCollectionSnapshot(ctx, httpAddr, collectionName)
		if err != nil {
			kb.Status.QdrantUpgradeState = "Failed"
			setCondition(&kb.Status.Conditions, metav1.Condition{
				Type:               conditionQdrantUpgrading,
				Status:             metav1.ConditionFalse,
				Reason:             "SnapshotFailed",
				Message:            fmt.Sprintf("Failed to create snapshot before upgrade: %v", err),
				LastTransitionTime: metav1.Now(),
			})
			return &upgradeResult{Requeue: true, RequeueAfter: 0}, nil
		}
		logger.Info("created pre-upgrade snapshot", "snapshot", snapshotName)

		// Patch StatefulSet image.
		newImage := nextVersion.Image(repo)
		if err := r.patchStatefulSetImage(ctx, sts, newImage); err != nil {
			return nil, fmt.Errorf("patching Qdrant StatefulSet image to %s: %w", newImage, err)
		}

		kb.Status.QdrantUpgradeState = "WaitingForReady"
		setCondition(&kb.Status.Conditions, metav1.Condition{
			Type:               conditionQdrantUpgrading,
			Status:             metav1.ConditionTrue,
			Reason:             "Upgrading",
			Message:            fmt.Sprintf("Upgrading Qdrant from %s to %s", current, nextVersion),
			LastTransitionTime: metav1.Now(),
		})

		return &upgradeResult{Requeue: true, RequeueAfter: qdrantUpgradeRequeueInterval}, nil
	}
}

// patchStatefulSetImage patches the first container's image in the StatefulSet.
func (r *KnowledgeBaseReconciler) patchStatefulSetImage(ctx context.Context, sts *appsv1.StatefulSet, image string) error {
	patch := client.MergeFrom(sts.DeepCopy())
	sts.Spec.Template.Spec.Containers[0].Image = image
	return r.Patch(ctx, sts, patch)
}

// qdrantPodCrashLooping checks if the Qdrant pod has been in CrashLoopBackOff
// for longer than the crash loop timeout threshold.
func (r *KnowledgeBaseReconciler) qdrantPodCrashLooping(ctx context.Context, sts *appsv1.StatefulSet) bool {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(sts.Namespace),
		client.MatchingLabels(sts.Spec.Selector.MatchLabels),
	); err != nil {
		return false
	}
	for _, pod := range pods.Items {
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name != "qdrant" {
				continue
			}
			if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
				if cs.LastTerminationState.Terminated != nil {
					terminatedAt := cs.LastTerminationState.Terminated.FinishedAt.Time
					if time.Since(terminatedAt) > qdrantUpgradeCrashLoopTimeout {
						return true
					}
				}
			}
		}
	}
	return false
}

// imageTag extracts the tag from a Docker image reference (e.g. "qdrant/qdrant:v1.17.1" → "v1.17.1").
func imageTag(image string) string {
	if i := strings.LastIndex(image, ":"); i >= 0 {
		return image[i+1:]
	}
	return "latest"
}

// imageRepo extracts the repository from a Docker image reference (e.g. "qdrant/qdrant:v1.17.1" → "qdrant/qdrant").
func imageRepo(image string) string {
	if i := strings.LastIndex(image, ":"); i >= 0 {
		return image[:i]
	}
	return image
}

// qdrantResourceName returns the k8s-safe name for the dedicated Qdrant StatefulSet
// and Service for a KnowledgeBase. Truncated to 63 chars (DNS label limit).
func qdrantResourceName(kbName string) string {
	safe := strings.Trim(dnsUnsafe.ReplaceAllString(strings.ToLower(kbName), "-"), "-")
	name := qdrantNamePrefix + safe
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-")
	}
	return name
}

// ensureCollection creates a Qdrant collection if it doesn't exist.
func (r *KnowledgeBaseReconciler) ensureCollection(ctx context.Context, qdrantURL, collectionName string, dimensions uint64) error {
	qClient, err := rag.NewQdrantClient(qdrantURL)
	if err != nil {
		return fmt.Errorf("connecting to qdrant: %w", err)
	}
	defer func() { _ = qClient.Close() }()

	return qClient.EnsureCollection(ctx, collectionName, dimensions)
}

// collectionStats returns point count and unique document count from Qdrant.
func (r *KnowledgeBaseReconciler) collectionStats(ctx context.Context, qdrantURL, collectionName string) (*rag.CollectionStats, error) {
	qClient, err := rag.NewQdrantClient(qdrantURL)
	if err != nil {
		return nil, fmt.Errorf("connecting to qdrant: %w", err)
	}
	defer func() { _ = qClient.Close() }()

	return qClient.CollectionStats(ctx, collectionName)
}

// estimateStoragePercent estimates the percentage of PVC storage used by Qdrant
// based on point count, vector dimensions, and the configured PVC size.
// Each point stores: vector (dims * 4 bytes) + payload (~1KB avg) + index overhead (~20%).
func (r *KnowledgeBaseReconciler) estimateStoragePercent(kb *agentorcav1alpha1.KnowledgeBase, dims uint64) int {
	if kb.Status.ChunkCount == 0 {
		return 0
	}

	storageSize := kb.Spec.VectorStore.StorageSize
	if storageSize == "" {
		storageSize = "10Gi"
	}
	capacity := resource.MustParse(storageSize)
	capacityBytes := capacity.Value()
	if capacityBytes == 0 {
		return 0
	}

	// Estimate: vector (dims * 4B float32) + payload (~1KB) + 20% overhead for HNSW index.
	bytesPerPoint := (dims * 4) + 1024
	estimatedBytes := int64(float64(uint64(kb.Status.ChunkCount)*bytesPerPoint) * 1.2)

	// Ceiling division so small collections show at least 1% instead of 0.
	pct := min(int((estimatedBytes*100+capacityBytes-1)/capacityBytes), 100)
	return pct
}

// runIngestion processes configured ingestion sources (ConfigMaps, URLs) and upserts documents.
// embedder must already be resolved and the dimension probed (kb.Status.EmbeddingDimensions > 0).
func (r *KnowledgeBaseReconciler) runIngestion(ctx context.Context, kb *agentorcav1alpha1.KnowledgeBase, embedder *rag.EmbeddingClient) error {
	logger := log.FromContext(ctx)

	if kb.Spec.Ingestion == nil {
		return nil
	}

	chunkCfg := rag.ChunkConfig{
		ChunkSize:    kb.Spec.Embedding.ChunkSize,
		ChunkOverlap: kb.Spec.Embedding.ChunkOverlap,
	}
	if chunkCfg.ChunkSize == 0 {
		chunkCfg = rag.DefaultChunkConfig()
	}

	dims := kb.Status.EmbeddingDimensions

	var docs []rag.Document

	// Ingest from ConfigMaps.
	for _, ref := range kb.Spec.Ingestion.ConfigMapRefs {
		var cm corev1.ConfigMap
		if err := r.Get(ctx, client.ObjectKey{Name: ref.Name, Namespace: kb.Namespace}, &cm); err != nil {
			logger.Error(err, "fetching ConfigMap for ingestion", "configMap", ref.Name)
			continue
		}
		for key, value := range cm.Data {
			docs = append(docs, rag.Document{
				ID:      fmt.Sprintf("configmap/%s/%s", ref.Name, key),
				Content: value,
				Metadata: map[string]any{
					"source":    "configmap",
					"configmap": ref.Name,
					"key":       key,
				},
			})
		}
	}

	// Ingest from URLs.
	for _, u := range kb.Spec.Ingestion.URLs {
		content, err := fetchURL(ctx, u)
		if err != nil {
			logger.Error(err, "fetching URL for ingestion", "url", u)
			continue
		}
		docs = append(docs, rag.Document{
			ID:      fmt.Sprintf("url/%s", u),
			Content: content,
			Metadata: map[string]any{
				"source": "url",
				"url":    u,
			},
		})
	}

	// Ingest from MCP sources.
	// MCP ingestion Jobs embed and write to Qdrant directly. We always call
	// runMCPIngestion so that we pick up completed/running Jobs. It is
	// idempotent: it only creates a new Job when none exists and the sync
	// interval has elapsed.
	mcpJobRunning := false
	for i, mcpSrc := range kb.Spec.Ingestion.MCP {
		done, err := r.runMCPIngestion(ctx, kb, mcpSrc, i)
		if err != nil {
			logger.Error(err, "MCP ingestion failed", "mcpServer", mcpSrc.MCPServerRef)
			continue
		}
		if !done {
			mcpJobRunning = true
		}
	}

	// Ingest ConfigMap/URL docs through the controller's own pipeline.
	if len(docs) > 0 {
		result, err := rag.IngestDocuments(ctx, docs, kb.Status.VectorStoreURL, kb.Status.CollectionName, uint64(dims), embedder, chunkCfg)
		if err != nil {
			return fmt.Errorf("ingesting documents: %w", err)
		}
		logger.Info("ConfigMap/URL ingestion complete", "docs", result.DocumentCount, "chunks", result.ChunkCount)
	}

	// ChunkCount is set by the caller from Qdrant directly (the single
	// source of truth). Here we just manage LastSyncTime.
	if !mcpJobRunning {
		now := metav1.Now()
		kb.Status.LastSyncTime = &now
	}
	return nil
}

// resolveEmbedder builds an EmbeddingClient from the KnowledgeBase's embedding config.
// On first use it resolves the provider from the ModelSelector and pins the choice to
// kb.Status.EmbeddingModelProvider so that all future calls use the same provider,
// preventing Qdrant dimension mismatches when the ModelSelector has multiple providers
// with different vector sizes.
func (r *KnowledgeBaseReconciler) resolveEmbedder(ctx context.Context, kb *agentorcav1alpha1.KnowledgeBase) (*rag.EmbeddingClient, error) {
	var mpName string
	if kb.Status.EmbeddingModelProvider != "" {
		// Already pinned — use the recorded provider directly.
		mpName = kb.Status.EmbeddingModelProvider
	} else {
		// First use — resolve from ModelSelector.
		var ms agentorcav1alpha1.ModelSelector
		if err := r.Get(ctx, client.ObjectKey{Name: kb.Spec.Embedding.ModelSelectorRef, Namespace: kb.Namespace}, &ms); err != nil {
			return nil, fmt.Errorf("getting ModelSelector %q: %w", kb.Spec.Embedding.ModelSelectorRef, err)
		}
		if len(ms.Spec.Providers) == 0 {
			return nil, fmt.Errorf("ModelSelector %q has no providers", kb.Spec.Embedding.ModelSelectorRef)
		}
		mpName = ms.Spec.Providers[0].Name
	}

	var mp agentorcav1alpha1.ModelProvider
	if err := r.Get(ctx, client.ObjectKey{Name: mpName, Namespace: kb.Namespace}, &mp); err != nil {
		return nil, fmt.Errorf("getting ModelProvider %q: %w", mpName, err)
	}

	endpoint := mp.Spec.BaseURL
	if endpoint == "" {
		endpoint = embeddingBaseURL(mp.Spec.LiteLLMModel)
	}

	// The operator reads the secret directly (it has RBAC for secrets).
	apiKey, err := r.readSecretKey(ctx, kb.Namespace, mp.Spec.CredentialsRef.Name, mp.Spec.CredentialsRef.Key)
	if err != nil {
		return nil, fmt.Errorf("reading credentials for %q: %w", mpName, err)
	}

	// Write key to a temp file for the embedding client.
	keyDir := fmt.Sprintf("/tmp/agentorca-embed-%s", mpName)
	if err := os.MkdirAll(keyDir, 0700); err != nil {
		return nil, err
	}
	keyFile := filepath.Join(keyDir, "api-key")
	if err := os.WriteFile(keyFile, []byte(apiKey), 0600); err != nil {
		return nil, err
	}

	model := mp.Spec.LiteLLMModel
	if idx := strings.Index(model, "/"); idx >= 0 {
		model = model[idx+1:]
	}

	// Pin to status on first use so all future calls use the same provider.
	// The field is written by the single Status().Update in Reconcile.
	if kb.Status.EmbeddingModelProvider == "" {
		kb.Status.EmbeddingModelProvider = mpName
		kb.Status.EmbeddingModel = mp.Spec.LiteLLMModel
	}

	return rag.NewEmbeddingClient(endpoint, keyFile, model, mp.Spec.DocPrompt, mp.Spec.QueryPrompt), nil
}

// embeddingBaseURL returns the provider's base URL for embedding calls.
// This is only used when the ModelProvider does not specify an explicit
// baseURL — i.e. the provider is a direct cloud API rather than a self-hosted
// LiteLLM proxy. The returned URL is appended with /v1/embeddings by the
// EmbeddingClient.
func embeddingBaseURL(litellmModel string) string {
	switch {
	case strings.HasPrefix(litellmModel, "openai/"):
		return "https://api.openai.com"
	case strings.HasPrefix(litellmModel, "gemini/"):
		return "https://generativelanguage.googleapis.com/v1beta/openai"
	case strings.HasPrefix(litellmModel, "ollama/"):
		return "http://localhost:11434"
	default:
		// Unknown provider — assume OpenAI-compatible endpoint.
		return "https://api.openai.com"
	}
}

// readSecretKey reads a single key from a Kubernetes Secret.
func (r *KnowledgeBaseReconciler) readSecretKey(ctx context.Context, namespace, name, key string) (string, error) {
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &secret); err != nil {
		return "", fmt.Errorf("getting secret %s/%s: %w", namespace, name, err)
	}
	val, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("key %q not found in secret %s/%s", key, namespace, name)
	}
	return string(val), nil
}

// fetchURL retrieves text content from a URL.
func fetchURL(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10MB limit
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	return string(body), nil
}

func durationFromSeconds(s int) time.Duration {
	return time.Duration(s) * time.Second
}

// knowledgeBasesForConfigMap maps a ConfigMap to the KnowledgeBases that reference it,
// so that a ConfigMap change triggers re-ingestion.
func (r *KnowledgeBaseReconciler) knowledgeBasesForConfigMap(ctx context.Context, obj client.Object) []reconcile.Request {
	cm := obj.(*corev1.ConfigMap)

	var kbList agentorcav1alpha1.KnowledgeBaseList
	if err := r.List(ctx, &kbList, client.InNamespace(cm.Namespace)); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, kb := range kbList.Items {
		if kb.Spec.Ingestion == nil {
			continue
		}
		for _, ref := range kb.Spec.Ingestion.ConfigMapRefs {
			if ref.Name == cm.Name {
				requests = append(requests, reconcile.Request{
					NamespacedName: client.ObjectKey{Name: kb.Name, Namespace: kb.Namespace},
				})
				break
			}
		}
	}
	return requests
}

// qdrantImageOrDefault returns the configured Qdrant image or the built-in default.
func (r *KnowledgeBaseReconciler) qdrantImageOrDefault() string {
	if r.QdrantImage != "" {
		return r.QdrantImage
	}
	return defaultQdrantImage()
}

// SetupWithManager registers the controller with the Manager.
func (r *KnowledgeBaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentorcav1alpha1.KnowledgeBase{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&batchv1.Job{}).
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(r.knowledgeBasesForConfigMap),
		).
		Watches(
			&agentorcav1alpha1.MCPServer{},
			handler.EnqueueRequestsFromMapFunc(r.knowledgeBasesForMCPServer),
		).
		Named("knowledgebase").
		Complete(r)
}
