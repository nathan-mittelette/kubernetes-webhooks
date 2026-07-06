package controllers

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// ManagedByLabelKey/Value mark a Secret as a managed replica: only
	// secrets carrying this label are ever updated or deleted by the
	// reconciler.
	ManagedByLabelKey   = "app.kubernetes.io/managed-by"
	ManagedByLabelValue = "docker-proxy-webhook"

	// SourceAnnotationKey records where a managed replica was copied from.
	SourceAnnotationKey = "docker-proxy-webhook.io/source"
	// SourceResourceVersionAnnotationKey records the source secret's
	// resourceVersion at the time of the last copy, so convergence can be
	// checked without re-comparing full secret data on every reconcile.
	SourceResourceVersionAnnotationKey = "docker-proxy-webhook.io/source-resource-version"

	// DisabledLabelKey/Value is the same opt-out label the mutating webhook
	// uses: a namespace carrying it is excluded from image rewriting, and
	// therefore does not need replicated pull secrets either.
	DisabledLabelKey   = "docker-proxy-webhook"
	DisabledLabelValue = "disabled"
)

var log = logf.Log.WithName("secret-replication-controller")

var (
	replicationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "docker_proxy_secret_replication_total",
			Help: "Successful replication operations",
		},
		[]string{"action", "target_namespace"},
	)
	replicationFailures = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "docker_proxy_secret_replication_failures_total",
			Help: "Source missing, API errors",
		},
		[]string{"reason", "target_namespace"},
	)
	replicationConflicts = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "docker_proxy_secret_replication_conflicts_total",
			Help: "Same-named unmanaged secret encountered",
		},
		[]string{"target_namespace"},
	)
)

func init() {
	metrics.Registry.MustRegister(replicationTotal, replicationFailures, replicationConflicts)
}

// SecretReplicationReconciler replicates a fixed set of secrets from
// SourceNamespace into every other namespace not excluded by the opt-out
// label, keeping copies in sync and repairing drift.
type SecretReplicationReconciler struct {
	client.Client
	SourceNamespace string
	SecretNames     []string
}

// Reconcile implements the namespace-at-a-time replication logic described
// in feature 05: skip excluded namespaces, then create/update/garbage-collect
// every configured secret in the target namespace.
func (r *SecretReplicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ns corev1.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: req.Name}, &ns); err != nil {
		if apierrors.IsNotFound(err) {
			// Deleted namespaces need no handling: their secrets die with them.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if shouldSkipNamespace(ns, r.SourceNamespace) {
		return ctrl.Result{}, nil
	}

	var errs []error
	for _, name := range r.SecretNames {
		if err := r.reconcileSecret(ctx, ns.Name, name); err != nil {
			errs = append(errs, err)
		}
	}
	if err := r.garbageCollect(ctx, ns.Name); err != nil {
		errs = append(errs, err)
	}

	return ctrl.Result{}, errors.Join(errs...)
}

// shouldSkipNamespace reports whether a namespace must never receive
// replicated secrets: it is terminating, is the source namespace itself, or
// carries the opt-out label.
func shouldSkipNamespace(ns corev1.Namespace, sourceNamespace string) bool {
	if ns.DeletionTimestamp != nil {
		return true
	}
	if ns.Name == sourceNamespace {
		return true
	}
	return ns.Labels[DisabledLabelKey] == DisabledLabelValue
}

// isManagedByUs reports whether secret was created by this controller
// (carries our managed-by label). Only managed secrets are ever updated or
// deleted.
func isManagedByUs(secret corev1.Secret) bool {
	return secret.Labels[ManagedByLabelKey] == ManagedByLabelValue
}

// needsUpdate reports whether a managed copy is stale relative to its
// source, based on the recorded source resourceVersion.
func needsUpdate(copy corev1.Secret, source corev1.Secret) bool {
	return copy.Annotations[SourceResourceVersionAnnotationKey] != source.ResourceVersion
}

// buildManagedCopy builds the Secret to create in targetNamespace from
// source, stamped with the ownership label/annotations.
func buildManagedCopy(source corev1.Secret, targetNamespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      source.Name,
			Namespace: targetNamespace,
			Labels: map[string]string{
				ManagedByLabelKey: ManagedByLabelValue,
			},
			Annotations: map[string]string{
				SourceAnnotationKey:                source.Namespace + "/" + source.Name,
				SourceResourceVersionAnnotationKey: source.ResourceVersion,
			},
		},
		Type: source.Type,
		Data: source.Data,
	}
}

// reconcileSecret ensures one configured secret is replicated (created,
// converged, or left alone if unmanaged) in targetNamespace.
func (r *SecretReplicationReconciler) reconcileSecret(ctx context.Context, targetNamespace, name string) error {
	var source corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: r.SourceNamespace, Name: name}, &source); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("source secret missing", "sourceNamespace", r.SourceNamespace, "name", name, "targetNamespace", targetNamespace)
			replicationFailures.WithLabelValues("source_missing", targetNamespace).Inc()
			return fmt.Errorf("source secret %s/%s not found", r.SourceNamespace, name)
		}
		replicationFailures.WithLabelValues("api_error", targetNamespace).Inc()
		return err
	}

	var target corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Namespace: targetNamespace, Name: name}, &target)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, buildManagedCopy(source, targetNamespace)); err != nil {
			replicationFailures.WithLabelValues("api_error", targetNamespace).Inc()
			return err
		}
		replicationTotal.WithLabelValues("create", targetNamespace).Inc()
		log.Info("created replicated secret", "namespace", targetNamespace, "name", name)
		return nil
	}
	if err != nil {
		replicationFailures.WithLabelValues("api_error", targetNamespace).Inc()
		return err
	}

	if !isManagedByUs(target) {
		log.Info("secret with this name already exists and is not managed by this controller; leaving it untouched", "namespace", targetNamespace, "name", name)
		replicationConflicts.WithLabelValues(targetNamespace).Inc()
		return nil
	}

	if !needsUpdate(target, source) {
		return nil
	}

	updated := target.DeepCopy()
	updated.Type = source.Type
	updated.Data = source.Data
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	updated.Annotations[SourceAnnotationKey] = source.Namespace + "/" + source.Name
	updated.Annotations[SourceResourceVersionAnnotationKey] = source.ResourceVersion
	if err := r.Update(ctx, updated); err != nil {
		replicationFailures.WithLabelValues("api_error", targetNamespace).Inc()
		return err
	}
	replicationTotal.WithLabelValues("update", targetNamespace).Inc()
	log.Info("updated replicated secret to match source", "namespace", targetNamespace, "name", name)
	return nil
}

// garbageCollect deletes managed copies in targetNamespace whose name is no
// longer part of the configured secret list. Only label-matched (managed)
// secrets are ever considered for deletion.
func (r *SecretReplicationReconciler) garbageCollect(ctx context.Context, targetNamespace string) error {
	var managed corev1.SecretList
	if err := r.List(ctx, &managed, client.InNamespace(targetNamespace), client.MatchingLabels{ManagedByLabelKey: ManagedByLabelValue}); err != nil {
		return err
	}

	var errs []error
	for _, secret := range managed.Items {
		if slices.Contains(r.SecretNames, secret.Name) {
			continue
		}
		if err := r.Delete(ctx, &secret); err != nil && !apierrors.IsNotFound(err) {
			replicationFailures.WithLabelValues("api_error", targetNamespace).Inc()
			errs = append(errs, err)
			continue
		}
		replicationTotal.WithLabelValues("delete", targetNamespace).Inc()
		log.Info("garbage collected orphaned managed secret", "namespace", targetNamespace, "name", secret.Name)
	}
	return errors.Join(errs...)
}

// mapSecretToNamespaceRequests implements the two secondary watches from
// feature 05 as a single map function: a change to a configured secret in
// the source namespace fans out to every namespace (rotation propagation),
// while a change to a managed copy re-enqueues only its own namespace (drift
// repair).
func (r *SecretReplicationReconciler) mapSecretToNamespaceRequests(ctx context.Context, obj client.Object) []reconcile.Request {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		return nil
	}

	if secret.Namespace == r.SourceNamespace {
		if !slices.Contains(r.SecretNames, secret.Name) {
			return nil
		}
		var namespaces corev1.NamespaceList
		if err := r.List(ctx, &namespaces); err != nil {
			log.Error(err, "unable to list namespaces to fan out source secret change")
			return nil
		}
		requests := make([]reconcile.Request, 0, len(namespaces.Items))
		for _, ns := range namespaces.Items {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: ns.Name}})
		}
		return requests
	}

	if isManagedByUs(*secret) {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: secret.Namespace}}}
	}
	return nil
}

// SetupWithManager wires the reconciler's namespace and secret watches.
func (r *SecretReplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Namespace{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.mapSecretToNamespaceRequests)).
		Named("secret-replication").
		Complete(r)
}
