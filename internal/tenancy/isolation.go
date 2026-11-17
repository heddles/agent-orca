package tenancy

import (
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

const (
	// LabelTenantID carries the owning tenant's ID on every tenant-owned object.
	// It is the isolation boundary: the API, the webhooks and the controllers
	// compare it, never the tenant name.
	LabelTenantID = "agentorca.io/tenant-id"
	// LabelScope marks an object as shared by the platform instead of owned by
	// one tenant.
	LabelScope = "agentorca.io/scope"
	// ScopePlatform is the LabelScope value for platform-shared objects. Tenants
	// can read and reference them; only platform admins can write them.
	ScopePlatform = "platform"
	// LabelTenantName is the older, display-only tenant name label. It is never
	// used to authorize; the backfill reads it to attribute objects that predate
	// tenant IDs.
	LabelTenantName = "agentorca.io/tenant"

	// ModeEnvVar selects the enforcement mode of the operator process.
	ModeEnvVar = "TENANT_ISOLATION_MODE"
)

// Mode says what happens to a label or reference violation.
type Mode string

const (
	// ModeAudit logs and counts violations and lets the request through. It is
	// the default, so a rollout shows what enforcement would break first.
	ModeAudit Mode = "audit"
	// ModeEnforce denies violations.
	ModeEnforce Mode = "enforce"
)

// ParseMode parses a mode name. The empty string is the default, audit.
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", string(ModeAudit):
		return ModeAudit, nil
	case string(ModeEnforce):
		return ModeEnforce, nil
	}
	return ModeAudit, fmt.Errorf("unknown tenant isolation mode %q: want %q or %q", s, ModeAudit, ModeEnforce)
}

var currentMode atomic.Value // Mode

// SetMode sets the process-wide enforcement mode.
func SetMode(m Mode) { currentMode.Store(m) }

// CurrentMode returns the enforcement mode; audit until SetMode is called.
func CurrentMode() Mode {
	if m, ok := currentMode.Load().(Mode); ok {
		return m
	}
	return ModeAudit
}

// Enforcing reports whether violations are denied.
func Enforcing() bool { return CurrentMode() == ModeEnforce }

// SetModeFromEnv reads ModeEnvVar. An unparsable value keeps the default and is
// returned as an error so the caller can log it.
func SetModeFromEnv() error {
	m, err := ParseMode(os.Getenv(ModeEnvVar))
	SetMode(m)
	return err
}

// Violation rule names, the "rule" metric label.
const (
	RuleLabelImmutable  = "label-immutable"
	RuleLabelMissing    = "label-missing"
	RuleLabelNamespace  = "label-namespace-mismatch"
	RuleCrossTenantRef  = "cross-tenant-reference"
	RuleScopeNotAdmin   = "platform-scope-needs-admin"
	RuleAPIUnlabelled   = "api-unlabelled-object"
	RuleBackfillUnknown = "backfill-unattributed"
)

var violations = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "agentorca_tenant_isolation_violations_total",
	Help: "Tenant label and reference violations, by rule, kind and mode (audit lets them through, enforce denies them).",
}, []string{"rule", "kind", "mode"})

func init() { ctrlmetrics.Registry.MustRegister(violations) }

// Violation records a violation: it logs and counts it, then reports whether
// the caller must deny. In audit mode it always returns false.
func Violation(rule, kind, namespace, name, detail string) (deny bool) {
	mode := CurrentMode()
	violations.WithLabelValues(rule, kind, string(mode)).Inc()
	slog.Warn("tenant isolation violation",
		"rule", rule, "kind", kind, "namespace", namespace, "name", name, "mode", string(mode), "detail", detail)
	return mode == ModeEnforce
}

// TenantIDOf is a TenantConfig's ID: the pinned one, else the mirrored one.
// Empty for a TenantConfig the reconciler has not registered yet.
func TenantIDOf(tc *agentorcav1alpha1.TenantConfig) string {
	if tc.Spec.TenantID != "" {
		return tc.Spec.TenantID
	}
	return tc.Status.TenantID
}

// NamespaceClaimants returns the IDs of the live TenantConfigs that list the
// namespace, and how many TenantConfigs claim it (a claimant with no ID yet is
// counted but has no ID to return).
func NamespaceClaimants(tcs []agentorcav1alpha1.TenantConfig, namespace string) (ids []string, claims int) {
	for i := range tcs {
		tc := &tcs[i]
		if !tc.DeletionTimestamp.IsZero() || !slices.Contains(tc.Spec.AllowedNamespaces, namespace) {
			continue
		}
		claims++
		if id := TenantIDOf(tc); id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids, claims
}

// TenantOf reads an object's ownership. id is empty when it has no tenant-id
// label; platform is true for platform-shared objects.
func TenantOf(obj metav1.Object) (id string, platform bool) {
	l := obj.GetLabels()
	return l[LabelTenantID], l[LabelScope] == ScopePlatform
}

// Stamp sets the tenant-id label on obj.
func Stamp(obj metav1.Object, tid string) {
	if tid == "" {
		return
	}
	l := obj.GetLabels()
	if l == nil {
		l = map[string]string{}
	}
	l[LabelTenantID] = tid
	obj.SetLabels(l)
}

// StripOwnership removes the tenant-id and scope labels. The API calls it on a
// request body before stamping so a caller cannot choose its own tenant or
// declare an object platform-shared.
func StripOwnership(obj metav1.Object) {
	l := obj.GetLabels()
	if l == nil {
		return
	}
	delete(l, LabelTenantID)
	delete(l, LabelScope)
	obj.SetLabels(l)
}

// InheritLabels copies the parent's ownership (tenant-id or platform scope)
// onto the child. It leaves the child alone when the parent has none, which is
// an object the backfill has not reached yet.
func InheritLabels(parent, child metav1.Object) {
	id, platform := TenantOf(parent)
	if id == "" && !platform {
		return
	}
	l := child.GetLabels()
	if l == nil {
		l = map[string]string{}
	}
	if id != "" {
		l[LabelTenantID] = id
	}
	if platform {
		l[LabelScope] = ScopePlatform
	}
	child.SetLabels(l)
}

// InheritTemplateLabels is InheritLabels for a pod template's metadata, so the
// pods a Job or StatefulSet creates carry the tenant too.
func InheritTemplateLabels(parent metav1.Object, tmpl *metav1.ObjectMeta) {
	InheritLabels(parent, tmpl)
}

// SetControllerReference makes owner the controller of child, as
// controllerutil.SetControllerReference does, and passes the owner's tenant
// ownership down to the child. Every controller-created child goes through it,
// so a child is never created without its parent's tenant.
func SetControllerReference(owner, child metav1.Object, scheme *runtime.Scheme, opts ...controllerutil.OwnerReferenceOption) error {
	if err := controllerutil.SetControllerReference(owner, child, scheme, opts...); err != nil {
		return err
	}
	InheritLabels(owner, child)
	return nil
}

// Access is how an API caller may use an object.
type Access int

const (
	// AccessNone hides the object: the API answers 404.
	AccessNone Access = iota
	// AccessOwn is an object of the caller's tenant: read and write.
	AccessOwn
	// AccessPlatform is a platform-shared object: read only.
	AccessPlatform
)

// Caller is the part of an API identity the access check needs.
type Caller struct {
	TenantID   string
	TenantName string
	Namespaces []string
}

// CheckAccess decides what a caller may do with obj. Both conditions must
// hold: the object belongs to the caller's tenant (or is platform-shared), and
// its namespace is one the caller may use. Cluster-scoped objects have no
// namespace to check.
//
// An object with no tenant-id label has no recorded owner. In enforce mode it
// is hidden. In audit mode the pre-ID rules apply (the namespace check, plus the
// display name label when present) and the gap is logged and counted.
func CheckAccess(c Caller, obj client.Object, kind string) Access {
	ns := obj.GetNamespace()
	if ns != "" && !slices.Contains(c.Namespaces, ns) {
		return AccessNone
	}
	id, platform := TenantOf(obj)
	switch {
	case platform:
		return AccessPlatform
	case id != "":
		if id == c.TenantID {
			return AccessOwn
		}
		return AccessNone
	}
	if name := obj.GetLabels()[LabelTenantName]; name != "" && name != c.TenantName {
		return AccessNone
	}
	if reportUnlabelledOnce(kind, ns, obj.GetName()) {
		return AccessNone
	}
	return AccessOwn
}

// maxUnlabelledSeen bounds the memory of the once-per-object report.
const maxUnlabelledSeen = 10000

var (
	unlabelledSeen  sync.Map
	unlabelledCount atomic.Int64
)

// reportUnlabelledOnce records an unlabelled object the API read, once per
// object per process: the same object is read on every list and poll, and
// counting each read would drown the metric. It reports whether the caller must
// deny, which depends only on the mode.
func reportUnlabelledOnce(kind, ns, name string) bool {
	key := kind + "/" + ns + "/" + name
	if _, dup := unlabelledSeen.LoadOrStore(key, struct{}{}); dup || unlabelledCount.Add(1) > maxUnlabelledSeen {
		return Enforcing()
	}
	return Violation(RuleAPIUnlabelled, kind, ns, name, "object has no "+LabelTenantID+" label")
}
