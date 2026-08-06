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
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	platformv1alpha1 "github.com/SuryaSaravanan4/configsync-operator/api/v1alpha1"
)

const (
	// managedByLabel marks a ConfigMap as created and owned by this controller.
	// The prune logic in Reconcile uses it to find ConfigMaps that belong to a
	// ConfigSync but sit in a namespace that is no longer targeted.
	managedByLabel = "platform.saravanan.dev/managed-by"

	// managedByValue is the only value managedByLabel is ever set to.
	managedByValue = "configsync-operator"

	// ownerLabel records the name of the ConfigSync that owns the ConfigMap.
	// The CEL rule on ConfigSync caps metadata.name at 63 characters so that it
	// is always a legal label value.
	ownerLabel = "platform.saravanan.dev/configsync"

	// conditionTypeReady is True when every target namespace holds a converged
	// ConfigMap.
	conditionTypeReady = "Ready"

	reasonAllNamespacesSynced = "AllNamespacesSynced"
	reasonConfigMapNotOwned   = "ConfigMapNotOwned"
	reasonSyncFailed          = "SyncFailed"

	// conflictRetryDelay is how long to wait before re-checking a namespace
	// blocked by a foreign ConfigMap. See the end of Reconcile for why a plain
	// watch cannot cover this case.
	conflictRetryDelay = time.Minute
)

// errNotOwned signals that a ConfigMap already occupies the name we want but
// does not carry a controller ownerRef pointing at our ConfigSync. It is a
// sentinel value compared with errors.Is, not a failure to retry.
var errNotOwned = errors.New("existing ConfigMap is not owned by this ConfigSync")

// ConfigSyncReconciler reconciles a ConfigSync object
type ConfigSyncReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=platform.saravanan.dev,resources=configsyncs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.saravanan.dev,resources=configsyncs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.saravanan.dev,resources=configsyncs/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete

// Reconcile materializes the ConfigSync's data as a ConfigMap in every target
// namespace. It is level-triggered: it does not know why it was invoked, only
// which ConfigSync to look at, so it re-derives the desired state from the spec
// on every call and makes the cluster match.
func (r *ConfigSyncReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var configSync platformv1alpha1.ConfigSync
	if err := r.Get(ctx, req.NamespacedName, &configSync); err != nil {
		if apierrors.IsNotFound(err) {
			// The ConfigSync is gone. Its ConfigMaps carry an ownerRef pointing
			// at it, so the garbage collector deletes them without our help.
			// Returning an error here would retry forever against an object
			// that will never come back.
			log.Info("ConfigSync not found, assuming it was deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	var (
		synced    []string
		conflicts []string
		failures  []string
	)

	// Every target namespace is attempted even if an earlier one fails. Bailing
	// out on the first error would make convergence depend on the order of
	// spec.targetNamespaces: a broken first namespace would stop a later one
	// from ever being repaired.
	for _, namespace := range configSync.Spec.TargetNamespaces {
		err := r.syncNamespace(ctx, &configSync, namespace)
		switch {
		case err == nil:
			synced = append(synced, namespace)
		case errors.Is(err, errNotOwned):
			log.Info("Refusing to adopt ConfigMap this controller did not create",
				"namespace", namespace, "name", configSync.Name)
			conflicts = append(conflicts, namespace)
		default:
			log.Error(err, "Failed to sync ConfigMap", "namespace", namespace, "name", configSync.Name)
			failures = append(failures, fmt.Sprintf("%s: %v", namespace, err))
		}
	}

	// ownerReferences cascade-delete our ConfigMaps when the ConfigSync goes
	// away, but they do nothing about spec drift: a namespace dropped from
	// targetNamespaces still has a live owner, so the garbage collector will
	// never touch it. Pruning by label is the only thing that removes it.
	failures = append(failures, r.pruneOrphans(ctx, &configSync, synced)...)

	if err := r.updateStatus(ctx, &configSync, synced, conflicts, failures); err != nil {
		return ctrl.Result{}, err
	}

	if len(failures) > 0 {
		// Non-nil error, so Result is deliberately left zero: controller-runtime
		// ignores RequeueAfter whenever the error is non-nil, and exponential
		// backoff is the right response to an unknown failure anyway.
		return ctrl.Result{}, fmt.Errorf("failed to converge %d namespace(s): %s",
			len(failures), strings.Join(failures, "; "))
	}

	if len(conflicts) > 0 {
		// A foreign ConfigMap carries no ownerRef pointing at us, so Owns()
		// cannot route its deletion back here. Without a poll we would stay
		// blocked until the next full resync (10h by default). Error must be nil
		// for RequeueAfter to take effect.
		return ctrl.Result{RequeueAfter: conflictRetryDelay}, nil
	}

	return ctrl.Result{}, nil
}

// syncNamespace makes the ConfigMap in one namespace match the ConfigSync,
// creating it if absent. It returns errNotOwned if a ConfigMap already holds the
// name without being ours, having written nothing.
func (r *ConfigSyncReconciler) syncNamespace(
	ctx context.Context,
	configSync *platformv1alpha1.ConfigSync,
	namespace string,
) error {
	// CreateOrUpdate needs an object carrying just the key. It Gets into this
	// object, then hands it to the mutate function either empty (create path) or
	// populated with the live state (update path).
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			// The managed ConfigMap is always named after the ConfigSync.
			Name:      configSync.Name,
			Namespace: namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, configMap, func() error {
		// The ownership check lives inside the mutate function on purpose. If it
		// ran before CreateOrUpdate, a foreign ConfigMap created in the window
		// between our check and CreateOrUpdate's own Get would get clobbered.
		// Here we are inspecting the exact object CreateOrUpdate is about to
		// write, so there is no window.
		//
		// A non-empty UID means the object came back from the server rather than
		// being the shell we just built, i.e. this is the update path.
		if configMap.UID != "" && !isOwnedBy(configMap, configSync) {
			return errNotOwned
		}

		// Merge our labels rather than replacing the map, so labels applied by
		// anyone else survive. A nil map must be initialised first: writing to a
		// nil Go map panics.
		if configMap.Labels == nil {
			configMap.Labels = map[string]string{}
		}
		configMap.Labels[managedByLabel] = managedByValue
		configMap.Labels[ownerLabel] = configSync.Name

		// Data is replaced wholesale, not merged. That is what makes a key
		// removed from the spec disappear from the ConfigMap.
		configMap.Data = maps.Clone(configSync.Spec.Data)

		// Safe to call on both paths: we have already established that any
		// existing controller ownerRef is ours.
		return controllerutil.SetControllerReference(configSync, configMap, r.Scheme)
	})

	return err
}

// pruneOrphans deletes ConfigMaps this ConfigSync owns that sit in namespaces it
// no longer targets. It returns a message per failed deletion rather than
// stopping, so one undeletable ConfigMap cannot block the rest.
func (r *ConfigSyncReconciler) pruneOrphans(
	ctx context.Context,
	configSync *platformv1alpha1.ConfigSync,
	keep []string,
) []string {
	log := logf.FromContext(ctx)

	var owned corev1.ConfigMapList
	if err := r.List(ctx, &owned, client.MatchingLabels{
		managedByLabel: managedByValue,
		ownerLabel:     configSync.Name,
	}); err != nil {
		return []string{fmt.Sprintf("listing managed ConfigMaps: %v", err)}
	}

	// map[string]struct{} is Go's set. struct{} occupies zero bytes, so the map
	// stores keys only.
	keepSet := make(map[string]struct{}, len(keep))
	for _, namespace := range keep {
		keepSet[namespace] = struct{}{}
	}

	var failures []string
	// Indexing rather than ranging over values avoids copying each ConfigMap out
	// of the list.
	for i := range owned.Items {
		configMap := &owned.Items[i]
		if _, ok := keepSet[configMap.Namespace]; ok {
			continue
		}
		// Labels are user-writable, so a match is a hint, not proof. Confirm
		// ownership before deleting anything.
		if !isOwnedBy(configMap, configSync) {
			continue
		}
		if err := r.Delete(ctx, configMap); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "Failed to prune ConfigMap", "namespace", configMap.Namespace, "name", configMap.Name)
			failures = append(failures, fmt.Sprintf("%s: prune: %v", configMap.Namespace, err))
			continue
		}
		log.Info("Pruned ConfigMap from namespace no longer targeted",
			"namespace", configMap.Namespace, "name", configMap.Name)
	}

	return failures
}

// updateStatus writes observedGeneration, the synced namespace list, and the
// Ready condition.
func (r *ConfigSyncReconciler) updateStatus(
	ctx context.Context,
	configSync *platformv1alpha1.ConfigSync,
	synced, conflicts, failures []string,
) error {
	// generation only advances when spec changes; writes through the status
	// subresource leave it alone. Recording it here is what lets a client tell
	// whether the status it is reading describes the spec it is looking at.
	configSync.Status.ObservedGeneration = configSync.Generation
	configSync.Status.SyncedNamespaces = synced

	condition := metav1.Condition{
		Type:               conditionTypeReady,
		ObservedGeneration: configSync.Generation,
	}
	switch {
	case len(failures) > 0:
		condition.Status = metav1.ConditionFalse
		condition.Reason = reasonSyncFailed
		condition.Message = fmt.Sprintf("Failed to converge: %s", strings.Join(failures, "; "))
	case len(conflicts) > 0:
		condition.Status = metav1.ConditionFalse
		condition.Reason = reasonConfigMapNotOwned
		condition.Message = fmt.Sprintf(
			"Skipped namespace(s) %s: a ConfigMap named %q already exists there and was not created by this controller",
			strings.Join(conflicts, ", "), configSync.Name)
	default:
		condition.Status = metav1.ConditionTrue
		condition.Reason = reasonAllNamespacesSynced
		condition.Message = fmt.Sprintf("ConfigMap synced to %d namespace(s)", len(synced))
	}

	// SetStatusCondition leaves LastTransitionTime untouched when Status has not
	// changed. That is what keeps repeated reconciles from producing a
	// different object every time, which would loop forever.
	meta.SetStatusCondition(&configSync.Status.Conditions, condition)

	// Status() targets the /status subresource, so this write cannot alter spec
	// and does not bump metadata.generation.
	return r.Status().Update(ctx, configSync)
}

// isOwnedBy reports whether obj carries a controller ownerRef pointing at
// configSync. UID is compared rather than name: a ConfigSync deleted and
// recreated under the same name is a different object, and its predecessor's
// ConfigMaps are not ours to touch.
func isOwnedBy(obj client.Object, configSync *platformv1alpha1.ConfigSync) bool {
	ref := metav1.GetControllerOf(obj)
	return ref != nil && ref.UID == configSync.UID
}

// SetupWithManager sets up the controller with the Manager.
func (r *ConfigSyncReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.ConfigSync{}).
		// Owns watches ConfigMaps and, for each event, reads the object's
		// controller ownerRef and enqueues a request for the *owning*
		// ConfigSync. This is what turns a hand-deleted ConfigMap into a
		// reconcile of its parent.
		Owns(&corev1.ConfigMap{}).
		Named("configsync").
		Complete(r)
}
