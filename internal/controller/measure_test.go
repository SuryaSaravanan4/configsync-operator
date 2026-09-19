//go:build measure

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

// This file measures how long one Reconcile takes as the number of target
// namespaces grows. It is excluded from the normal suite and from CI because it
// takes about eight minutes. Run it with:
//
//	KUBEBUILDER_ASSETS=... go test ./internal/controller/ -tags measure \
//	    -ginkgo.focus MEASURE -ginkgo.v -timeout 20m
//
// and read the lines that start with MEASURE. See docs/measurements.md for what
// the numbers do and do not show.
import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	platformv1alpha1 "github.com/SuryaSaravanan4/configsync-operator/api/v1alpha1"
)

// nolint: this is a measurement harness, not a test of behaviour.
var _ = Describe("MEASURE reconcile cost", Ordered, func() {
	const reps = 7
	sizes := []int{1, 10, 50, 100}

	seq := 0
	var (
		mgr  ctrl.Manager
		stop context.CancelFunc
	)

	BeforeAll(func() {
		var err error
		mgr, err = ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme.Scheme, Metrics: metricsserver.Options{BindAddress: "0"}})
		Expect(err).NotTo(HaveOccurred())
		mctx, cancel := context.WithCancel(ctx)
		stop = cancel
		go func() { defer GinkgoRecover(); _ = mgr.Start(mctx) }()
		Expect(mgr.GetCache().WaitForCacheSync(mctx)).To(BeTrue())
		for i := 0; i < 100; i++ {
			Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("meas-ns-%d", i)}})).To(Succeed())
		}
	})
	AfterAll(func() { stop() })

	median := func(d []time.Duration) (med, lo, hi float64) {
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		ms := func(x time.Duration) float64 { return float64(x.Microseconds()) / 1000 }
		return ms(d[len(d)/2]), ms(d[0]), ms(d[len(d)-1])
	}

	type variant struct {
		name  string
		quiet bool
		fake  bool
	}
	variants := []variant{
		{"real-recorder+dev-logging", false, false},
		{"real-recorder+no-logging", true, false},
		{"fake-recorder+no-logging", true, true},
	}

	run := func(v variant, scenario string, n int, missing bool) (first, repeat []time.Duration) {
		var rec events.EventRecorder = mgr.GetEventRecorder("measure")
		if v.fake {
			rec = events.NewFakeRecorder(100000)
		}
		r := &ConfigSyncReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Recorder: rec}
		rctx := ctx
		if v.quiet {
			rctx = logr.NewContext(ctx, logr.Discard())
		}
		for rep := 0; rep < reps+1; rep++ { // rep 0 is a discarded warm-up
			seq++
			name := fmt.Sprintf("meas-%s-%d", scenario, seq)
			targets := make([]string, n)
			for i := range targets {
				if missing {
					targets[i] = fmt.Sprintf("meas-missing-%d", i)
				} else {
					targets[i] = fmt.Sprintf("meas-ns-%d", i)
				}
			}
			cs := &platformv1alpha1.ConfigSync{ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec: platformv1alpha1.ConfigSyncSpec{TargetNamespaces: targets, Data: map[string]string{"K": "v"}}}
			Expect(k8sClient.Create(ctx, cs)).To(Succeed())
			key := types.NamespacedName{Name: name}
			Eventually(func() error { return mgr.GetClient().Get(ctx, key, &platformv1alpha1.ConfigSync{}) }, 10*time.Second).Should(Succeed())

			t0 := time.Now()
			_, _ = r.Reconcile(rctx, ctrl.Request{NamespacedName: key})
			d1 := time.Since(t0)

			// Let the cache catch up so the repeat reads what the first pass wrote.
			Eventually(func(g Gomega) {
				var got platformv1alpha1.ConfigSync
				g.Expect(mgr.GetClient().Get(ctx, key, &got)).To(Succeed())
				g.Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
				if !missing {
					var cm corev1.ConfigMap
					g.Expect(mgr.GetClient().Get(ctx, types.NamespacedName{Namespace: targets[n-1], Name: name}, &cm)).To(Succeed())
				}
			}, 15*time.Second, 20*time.Millisecond).Should(Succeed())

			t1 := time.Now()
			_, _ = r.Reconcile(rctx, ctrl.Request{NamespacedName: key})
			d2 := time.Since(t1)

			if rep > 0 {
				first, repeat = append(first, d1), append(repeat, d2)
			}
			Expect(k8sClient.Delete(ctx, cs)).To(Succeed())
		}
		return first, repeat
	}

	It("measures", func() {
		for _, v := range variants {
			for _, n := range sizes {
				f, rp := run(v, "ok", n, false)
				m, lo, hi := median(f)
				fmt.Fprintf(GinkgoWriter, "MEASURE %-27s healthy N=%-3d first-reconcile(create) median=%8.1fms min=%8.1f max=%8.1f\n", v.name, n, m, lo, hi)
				m, lo, hi = median(rp)
				fmt.Fprintf(GinkgoWriter, "MEASURE %-27s healthy N=%-3d steady-state(no-op)     median=%8.1fms min=%8.1f max=%8.1f\n", v.name, n, m, lo, hi)
				f, rp = run(v, "miss", n, true)
				m, lo, hi = median(f)
				fmt.Fprintf(GinkgoWriter, "MEASURE %-27s missing N=%-3d first-reconcile         median=%8.1fms min=%8.1f max=%8.1f\n", v.name, n, m, lo, hi)
				m, lo, hi = median(rp)
				fmt.Fprintf(GinkgoWriter, "MEASURE %-27s missing N=%-3d repeat-reconcile        median=%8.1fms min=%8.1f max=%8.1f\n", v.name, n, m, lo, hi)
			}
		}
	})
})
