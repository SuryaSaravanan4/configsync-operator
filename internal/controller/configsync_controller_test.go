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
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1alpha1 "github.com/SuryaSaravanan4/configsync-operator/api/v1alpha1"
)

// envtest runs only etcd and kube-apiserver, not kube-controller-manager. Two
// things follow, and they shape every test below:
//
//   - There is no garbage collector, so ownerReferences are stored but never
//     acted upon. Cascade deletion of ConfigMaps when a ConfigSync is removed
//     therefore cannot be covered here; it is verified by hand against kind.
//   - There is no namespace lifecycle controller, so a deleted Namespace stays
//     Terminating forever. Tests never delete namespaces; each one allocates
//     fresh names instead.
//
// k8sClient is a direct (uncached) client, so a read immediately after a write
// is consistent and no Eventually polling is needed.

// Literals shared across specs, extracted so that the linter's goconst check
// stays happy and the intent of each is named.
const (
	testDataKey      = "KEY"
	testDataValue    = "value"
	preciousKey      = "PRECIOUS"
	doNotTouchValue  = "do-not-touch"
	defaultNamespace = "default"
)

var _ = Describe("ConfigSync Controller", func() {
	var (
		reconciler *ConfigSyncReconciler
		// uniqueID makes every ConfigSync and Namespace name distinct across
		// specs. ConfigSync is cluster-scoped and namespaces cannot be cleaned
		// up, so leftover objects from one spec would otherwise leak into the
		// next.
		uniqueID int
	)

	BeforeEach(func() {
		reconciler = &ConfigSyncReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		uniqueID++
	})

	// newName returns a name unique to the running spec.
	newName := func(prefix string) string {
		return fmt.Sprintf("%s-%d", prefix, uniqueID)
	}

	// createNamespaces creates the given namespaces and returns their names.
	createNamespaces := func(names ...string) []string {
		for _, name := range names {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		}
		return names
	}

	// createConfigSync creates a ConfigSync and returns the live object, so that
	// tests have its UID and generation.
	createConfigSync := func(name string, data map[string]string, namespaces ...string) *platformv1alpha1.ConfigSync {
		configSync := &platformv1alpha1.ConfigSync{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: platformv1alpha1.ConfigSyncSpec{
				TargetNamespaces: namespaces,
				Data:             data,
			},
		}
		Expect(k8sClient.Create(ctx, configSync)).To(Succeed())
		return configSync
	}

	// reconcileOnce drives a single reconcile of the named ConfigSync and
	// asserts it did not fail.
	reconcileOnce := func(name string) reconcile.Result {
		result, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: name},
		})
		Expect(err).NotTo(HaveOccurred())
		return result
	}

	// getConfigMap fetches a ConfigMap, failing the spec if it is absent.
	getConfigMap := func(namespace, name string) *corev1.ConfigMap {
		configMap := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, configMap)).To(Succeed())
		return configMap
	}

	// getConfigSync re-reads a ConfigSync so that status written by the
	// reconciler is visible.
	getConfigSync := func(name string) *platformv1alpha1.ConfigSync {
		configSync := &platformv1alpha1.ConfigSync{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, configSync)).To(Succeed())
		return configSync
	}

	Context("creating ConfigMaps", func() {
		It("materializes a ConfigMap in every target namespace with labels and a controller ownerRef", func() {
			name := newName("basic")
			namespaces := createNamespaces(newName("ns-a"), newName("ns-b"))
			configSync := createConfigSync(name, map[string]string{testDataKey: testDataValue, "OTHER": "thing"}, namespaces...)

			reconcileOnce(name)

			for _, namespace := range namespaces {
				configMap := getConfigMap(namespace, name)

				By("carrying the spec data verbatim in " + namespace)
				Expect(configMap.Data).To(Equal(map[string]string{testDataKey: testDataValue, "OTHER": "thing"}))

				By("carrying both tracking labels in " + namespace)
				Expect(configMap.Labels).To(HaveKeyWithValue(managedByLabel, managedByValue))
				Expect(configMap.Labels).To(HaveKeyWithValue(ownerLabel, name))

				// This ownerRef is load-bearing twice over: the garbage collector
				// uses it to cascade-delete, and Owns() uses it to route
				// ConfigMap events back to the parent ConfigSync.
				By("carrying a controller ownerRef pointing at the ConfigSync in " + namespace)
				ownerRef := metav1.GetControllerOf(configMap)
				Expect(ownerRef).NotTo(BeNil())
				Expect(ownerRef.Kind).To(Equal("ConfigSync"))
				Expect(ownerRef.Name).To(Equal(name))
				Expect(ownerRef.UID).To(Equal(configSync.UID))
			}
		})

		It("writes nothing on a second reconcile of unchanged state", func() {
			// The regression test for the naive Create: reconciling again must be
			// not merely error-free but write-free. A ConfigMap write bumps
			// resourceVersion, and because Owns() feeds our own writes back to us
			// as events, any unconditional write is a self-sustaining loop.
			name := newName("idempotent")
			namespaces := createNamespaces(newName("ns"))
			createConfigSync(name, map[string]string{testDataKey: testDataValue}, namespaces...)

			reconcileOnce(name)
			before := getConfigMap(namespaces[0], name).ResourceVersion

			reconcileOnce(name)
			after := getConfigMap(namespaces[0], name).ResourceVersion

			Expect(after).To(Equal(before), "reconcile issued a write when nothing had changed")
		})

		It("recreates a ConfigMap deleted out from under it", func() {
			name := newName("recreate")
			namespaces := createNamespaces(newName("ns"))
			createConfigSync(name, map[string]string{testDataKey: testDataValue}, namespaces...)

			reconcileOnce(name)
			originalUID := getConfigMap(namespaces[0], name).UID

			By("deleting the ConfigMap by hand")
			Expect(k8sClient.Delete(ctx, getConfigMap(namespaces[0], name))).To(Succeed())

			reconcileOnce(name)

			recreated := getConfigMap(namespaces[0], name)
			Expect(recreated.UID).NotTo(Equal(originalUID), "expected a genuinely new object")
			Expect(recreated.Data).To(Equal(map[string]string{testDataKey: testDataValue}))
		})
	})

	Context("converging drifted ConfigMaps", func() {
		It("restores changed values and removes keys not present in the spec", func() {
			name := newName("drift")
			namespaces := createNamespaces(newName("ns"))
			createConfigSync(name, map[string]string{testDataKey: testDataValue}, namespaces...)
			reconcileOnce(name)

			By("tampering with the ConfigMap data")
			configMap := getConfigMap(namespaces[0], name)
			configMap.Data = map[string]string{testDataKey: "tampered", "INJECTED": "unwanted"}
			Expect(k8sClient.Update(ctx, configMap)).To(Succeed())

			reconcileOnce(name)

			// Data is assigned wholesale rather than merged, which is what makes
			// removal converge as well as modification.
			Expect(getConfigMap(namespaces[0], name).Data).To(Equal(map[string]string{testDataKey: testDataValue}))
		})

		It("keeps labels applied by other actors", func() {
			// The mirror image of the previous test: Data is replaced, but labels
			// are merged, so the controller is not destructive to metadata it
			// does not own.
			name := newName("labels")
			namespaces := createNamespaces(newName("ns"))
			createConfigSync(name, map[string]string{testDataKey: testDataValue}, namespaces...)
			reconcileOnce(name)

			By("adding an unrelated label")
			configMap := getConfigMap(namespaces[0], name)
			configMap.Labels["example.com/added-by-someone-else"] = "keep-me"
			Expect(k8sClient.Update(ctx, configMap)).To(Succeed())

			reconcileOnce(name)

			converged := getConfigMap(namespaces[0], name)
			Expect(converged.Labels).To(HaveKeyWithValue("example.com/added-by-someone-else", "keep-me"))
			Expect(converged.Labels).To(HaveKeyWithValue(managedByLabel, managedByValue))
			Expect(converged.Labels).To(HaveKeyWithValue(ownerLabel, name))
		})
	})

	Context("pruning", func() {
		It("deletes ConfigMaps in namespaces removed from targetNamespaces", func() {
			// ownerReferences cannot do this. A namespace dropped from the spec
			// still has a live owner, so the garbage collector will never
			// reclaim it. Label-based pruning is the only mechanism that does.
			name := newName("prune")
			namespaces := createNamespaces(newName("ns-keep"), newName("ns-drop"))
			keep, drop := namespaces[0], namespaces[1]

			createConfigSync(name, map[string]string{testDataKey: testDataValue}, keep, drop)
			reconcileOnce(name)
			Expect(getConfigMap(drop, name)).NotTo(BeNil())

			// Re-read before updating: the reconcile above wrote status, which
			// bumped resourceVersion, so the object returned by createConfigSync
			// is now stale and Update would be rejected with a 409.
			By("narrowing targetNamespaces to drop one namespace")
			configSync := getConfigSync(name)
			configSync.Spec.TargetNamespaces = []string{keep}
			Expect(k8sClient.Update(ctx, configSync)).To(Succeed())

			reconcileOnce(name)

			By("removing the ConfigMap from the dropped namespace")
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: drop, Name: name}, &corev1.ConfigMap{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "ConfigMap in dropped namespace was not pruned")

			By("leaving the still-targeted namespace alone")
			Expect(getConfigMap(keep, name).Data).To(Equal(map[string]string{testDataKey: testDataValue}))
		})

		It("does not delete a ConfigMap that carries our labels but not our ownerRef", func() {
			// Labels are writable by anyone, so a label match is a hint and not
			// proof of ownership. Without the ownership check in pruneOrphans,
			// applying two labels to any ConfigMap in the cluster would be enough
			// to make this controller delete it on someone else's behalf.
			name := newName("forged")
			namespaces := createNamespaces(newName("ns-target"), newName("ns-elsewhere"))
			target, elsewhere := namespaces[0], namespaces[1]

			createConfigSync(name, map[string]string{testDataKey: testDataValue}, target)

			By("planting a ConfigMap that forges our labels in an untargeted namespace")
			forged := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: elsewhere,
					Labels: map[string]string{
						managedByLabel: managedByValue,
						ownerLabel:     name,
					},
				},
				Data: map[string]string{preciousKey: "do-not-delete"},
			}
			Expect(k8sClient.Create(ctx, forged)).To(Succeed())

			reconcileOnce(name)

			survivor := getConfigMap(elsewhere, name)
			Expect(survivor.Data).To(Equal(map[string]string{preciousKey: "do-not-delete"}))
			Expect(survivor.UID).To(Equal(forged.UID))
		})
	})

	Context("refusing to adopt ConfigMaps it did not create", func() {
		It("leaves a pre-existing ConfigMap completely untouched", func() {
			name := newName("foreign")
			namespaces := createNamespaces(newName("ns"))

			By("planting a ConfigMap that already occupies the name")
			foreign := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: namespaces[0],
					Labels:    map[string]string{"owner": "someone-else"},
				},
				Data: map[string]string{preciousKey: doNotTouchValue},
			}
			Expect(k8sClient.Create(ctx, foreign)).To(Succeed())

			createConfigSync(name, map[string]string{testDataKey: testDataValue}, namespaces...)
			reconcileOnce(name)

			untouched := getConfigMap(namespaces[0], name)
			Expect(untouched.UID).To(Equal(foreign.UID))
			Expect(untouched.Data).To(Equal(map[string]string{preciousKey: doNotTouchValue}))
			Expect(untouched.Labels).To(Equal(map[string]string{"owner": "someone-else"}))
			Expect(untouched.OwnerReferences).To(BeEmpty(), "controller adopted a ConfigMap it did not create")
			Expect(untouched.ResourceVersion).To(Equal(foreign.ResourceVersion), "controller wrote to a foreign ConfigMap")
		})

		It("reports the refusal as Ready=False and omits the namespace from syncedNamespaces", func() {
			// Refusing quietly would be a bug: the user needs to know why a
			// namespace is not converging.
			name := newName("conflict-status")
			namespaces := createNamespaces(newName("ns-ok"), newName("ns-blocked"))
			ok, blocked := namespaces[0], namespaces[1]

			Expect(k8sClient.Create(ctx, &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: blocked},
				Data:       map[string]string{preciousKey: doNotTouchValue},
			})).To(Succeed())

			createConfigSync(name, map[string]string{testDataKey: testDataValue}, ok, blocked)
			result := reconcileOnce(name)

			By("requeueing, because a foreign ConfigMap's deletion cannot reach us via Owns()")
			Expect(result.RequeueAfter).To(Equal(conflictRetryDelay))

			status := getConfigSync(name).Status
			Expect(status.SyncedNamespaces).To(ConsistOf(ok))
			Expect(status.SyncedNamespaces).NotTo(ContainElement(blocked))

			condition := meta.FindStatusCondition(status.Conditions, conditionTypeReady)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal(reasonConfigMapNotOwned))
			Expect(condition.Message).To(ContainSubstring(blocked))
		})
	})

	Context("status", func() {
		It("sets Ready=True with observedGeneration matching the spec generation", func() {
			name := newName("status-ok")
			namespaces := createNamespaces(newName("ns-a"), newName("ns-b"))
			configSync := createConfigSync(name, map[string]string{testDataKey: testDataValue}, namespaces...)

			reconcileOnce(name)

			updated := getConfigSync(name)
			Expect(updated.Status.ObservedGeneration).To(Equal(configSync.Generation))
			Expect(updated.Status.SyncedNamespaces).To(ConsistOf(namespaces[0], namespaces[1]))

			condition := meta.FindStatusCondition(updated.Status.Conditions, conditionTypeReady)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
			Expect(condition.Reason).To(Equal(reasonAllNamespacesSynced))
			Expect(condition.ObservedGeneration).To(Equal(configSync.Generation))
		})

		It("writes no status update when the computed status is unchanged", func() {
			// The infinite-loop guard, and the reason it works. A status write
			// bumps resourceVersion, which fires a ConfigSync event, which
			// triggers another reconcile. That terminates only because the
			// computed status is byte-identical, so the API server treats the
			// update as a no-op and emits no event. Building the condition with
			// metav1.Now() instead of letting meta.SetStatusCondition preserve
			// LastTransitionTime would make every reconcile a real write and spin
			// the controller forever.
			//
			// The sleep is load-bearing. metav1.Time serialises at one-second
			// granularity, so reconciles inside the same second produce an
			// identical timestamp even when the code is wrong. Crossing a second
			// boundary is what makes this assertion able to fail.
			name := newName("no-loop")
			namespaces := createNamespaces(newName("ns"))
			createConfigSync(name, map[string]string{testDataKey: testDataValue}, namespaces...)

			reconcileOnce(name)
			settled := getConfigSync(name)
			first := meta.FindStatusCondition(settled.Status.Conditions, conditionTypeReady)
			Expect(first).NotTo(BeNil())

			time.Sleep(1100 * time.Millisecond)
			reconcileOnce(name)

			after := getConfigSync(name)

			// The invariant that actually matters: no write occurred at all.
			Expect(after.ResourceVersion).To(Equal(settled.ResourceVersion),
				"reconcile wrote status when nothing had changed, which self-triggers forever")

			last := meta.FindStatusCondition(after.Status.Conditions, conditionTypeReady)
			Expect(last).NotTo(BeNil())
			Expect(last.LastTransitionTime).To(Equal(first.LastTransitionTime),
				"lastTransitionTime moved without the condition changing")
		})
	})

	Context("when the ConfigSync does not exist", func() {
		It("returns without error rather than retrying forever", func() {
			// A deleted ConfigSync is a normal outcome, not a failure. Returning
			// an error would put a nonexistent object into permanent exponential
			// backoff.
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: newName("absent")},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
		})
	})

	Context("API validation enforced by the CRD schema", func() {
		It("rejects a name longer than 63 characters and accepts one of exactly 63", func() {
			// metadata.name is copied into a label value, and label values are
			// capped at 63 characters. The CEL rule turns what would be a
			// confusing mid-reconcile write failure into an admission error.
			tooLong := strings.Repeat("a", 64)
			err := k8sClient.Create(ctx, &platformv1alpha1.ConfigSync{
				ObjectMeta: metav1.ObjectMeta{Name: tooLong},
				Spec: platformv1alpha1.ConfigSyncSpec{
					TargetNamespaces: []string{defaultNamespace},
					Data:             map[string]string{testDataKey: testDataValue},
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("63 characters or fewer"))

			atLimit := strings.Repeat("b", 63)
			Expect(k8sClient.Create(ctx, &platformv1alpha1.ConfigSync{
				ObjectMeta: metav1.ObjectMeta{Name: atLimit},
				Spec: platformv1alpha1.ConfigSyncSpec{
					TargetNamespaces: []string{defaultNamespace},
					Data:             map[string]string{testDataKey: testDataValue},
				},
			})).To(Succeed())
		})

		It("rejects an empty targetNamespaces and an empty data map", func() {
			By("rejecting empty targetNamespaces")
			err := k8sClient.Create(ctx, &platformv1alpha1.ConfigSync{
				ObjectMeta: metav1.ObjectMeta{Name: newName("no-namespaces")},
				Spec: platformv1alpha1.ConfigSyncSpec{
					TargetNamespaces: []string{},
					Data:             map[string]string{testDataKey: testDataValue},
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("targetNamespaces"))

			By("rejecting empty data")
			err = k8sClient.Create(ctx, &platformv1alpha1.ConfigSync{
				ObjectMeta: metav1.ObjectMeta{Name: newName("no-data")},
				Spec: platformv1alpha1.ConfigSyncSpec{
					TargetNamespaces: []string{defaultNamespace},
					Data:             map[string]string{},
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("data"))
		})
	})
})
