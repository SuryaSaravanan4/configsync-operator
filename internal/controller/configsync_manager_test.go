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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	platformv1alpha1 "github.com/SuryaSaravanan4/configsync-operator/api/v1alpha1"
)

// The specs in configsync_controller_test.go call Reconcile by hand, so they
// prove what one reconcile does but not that anything ever calls it. These specs
// run a real manager against envtest and never call Reconcile themselves: they
// prove the watch wiring in SetupWithManager (For + Owns) turns API events into
// reconciles.
//
// The manager runs only for the duration of this container. Running it for the
// whole suite would have it reconcile the ConfigSyncs the other specs create,
// racing their direct Reconcile calls and resourceVersion assertions.
var _ = Describe("ConfigSync controller wiring", Ordered, func() {
	const (
		eventuallyTimeout = 15 * time.Second
		pollInterval      = 200 * time.Millisecond
	)

	var (
		stopManager context.CancelFunc
		managerDone chan struct{}
	)

	BeforeAll(func() {
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:  scheme.Scheme,
			Metrics: metricsserver.Options{BindAddress: "0"},
		})
		Expect(err).NotTo(HaveOccurred())

		Expect((&ConfigSyncReconciler{
			Client:   mgr.GetClient(),
			Scheme:   mgr.GetScheme(),
			Recorder: mgr.GetEventRecorder("configsync-controller"),
		}).SetupWithManager(mgr)).To(Succeed())

		managerCtx, cancel := context.WithCancel(ctx)
		stopManager = cancel
		managerDone = make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(managerDone)
			Expect(mgr.Start(managerCtx)).To(Succeed())
		}()
	})

	AfterAll(func() {
		stopManager()
		Eventually(managerDone, eventuallyTimeout).Should(BeClosed())
	})

	// setup creates a namespace and a ConfigSync targeting it, and waits for the
	// controller, not the test, to materialise the ConfigMap.
	setup := func(name string) (namespace string) {
		namespace = name + "-ns"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
		Expect(k8sClient.Create(ctx, &platformv1alpha1.ConfigSync{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: platformv1alpha1.ConfigSyncSpec{
				TargetNamespaces: []string{namespace},
				Data:             map[string]string{testDataKey: testDataValue},
			},
		})).To(Succeed())

		Eventually(func(g Gomega) {
			var configMap corev1.ConfigMap
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &configMap)).To(Succeed())
			g.Expect(configMap.Data).To(Equal(map[string]string{testDataKey: testDataValue}))
		}, eventuallyTimeout, pollInterval).Should(Succeed())
		return namespace
	}

	It("materialises ConfigMaps and reports Ready when a ConfigSync is created", func() {
		name := "wiring-create"
		setup(name)

		Eventually(func(g Gomega) {
			var configSync platformv1alpha1.ConfigSync
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, &configSync)).To(Succeed())
			condition := meta.FindStatusCondition(configSync.Status.Conditions, conditionTypeReady)
			g.Expect(condition).NotTo(BeNil())
			g.Expect(condition.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(configSync.Status.ObservedGeneration).To(Equal(configSync.Generation))
		}, eventuallyTimeout, pollInterval).Should(Succeed())
	})

	It("recreates a ConfigMap deleted by hand, via the Owns() watch", func() {
		name := "wiring-delete"
		namespace := setup(name)
		key := types.NamespacedName{Namespace: namespace, Name: name}

		var original corev1.ConfigMap
		Expect(k8sClient.Get(ctx, key, &original)).To(Succeed())
		Expect(k8sClient.Delete(ctx, &original)).To(Succeed())

		Eventually(func(g Gomega) {
			var recreated corev1.ConfigMap
			g.Expect(k8sClient.Get(ctx, key, &recreated)).To(Succeed())
			g.Expect(recreated.UID).NotTo(Equal(original.UID), "still the deleted object")
			g.Expect(recreated.Data).To(Equal(map[string]string{testDataKey: testDataValue}))
		}, eventuallyTimeout, pollInterval).Should(Succeed())
	})

	It("reverts a hand-edited ConfigMap back to the spec, via the Owns() watch", func() {
		name := "wiring-drift"
		namespace := setup(name)
		key := types.NamespacedName{Namespace: namespace, Name: name}

		var configMap corev1.ConfigMap
		Expect(k8sClient.Get(ctx, key, &configMap)).To(Succeed())
		configMap.Data = map[string]string{testDataKey: "tampered", "INJECTED": "unwanted"}
		Expect(k8sClient.Update(ctx, &configMap)).To(Succeed())

		Eventually(func(g Gomega) {
			var converged corev1.ConfigMap
			g.Expect(k8sClient.Get(ctx, key, &converged)).To(Succeed())
			g.Expect(converged.Data).To(Equal(map[string]string{testDataKey: testDataValue}))
		}, eventuallyTimeout, pollInterval).Should(Succeed())
	})

	It("applies a spec change to existing ConfigMaps, via the For() watch", func() {
		name := "wiring-spec"
		namespace := setup(name)

		Eventually(func(g Gomega) {
			var configSync platformv1alpha1.ConfigSync
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, &configSync)).To(Succeed())
			configSync.Spec.Data = map[string]string{testDataKey: "v2"}
			g.Expect(k8sClient.Update(ctx, &configSync)).To(Succeed())
		}, eventuallyTimeout, pollInterval).Should(Succeed())

		Eventually(func(g Gomega) {
			var configMap corev1.ConfigMap
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &configMap)).To(Succeed())
			g.Expect(configMap.Data).To(Equal(map[string]string{testDataKey: "v2"}))
		}, eventuallyTimeout, pollInterval).Should(Succeed())
	})
})
