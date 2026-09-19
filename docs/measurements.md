# Reconcile cost measurements

What one `Reconcile` costs as the number of target namespaces grows, and what
that does and does not tell you. Reproduce with the harness in
`internal/controller/measure_test.go` (build tag `measure`; not run by CI).
Raw output is in [measurements-raw.txt](measurements-raw.txt).

## Conditions, so the numbers are read correctly

- **Where:** envtest (a local `etcd` and `kube-apiserver` 1.36.2, no
  controller-manager, no kubelet) on one Windows 11 laptop under WSL2:
  12th Gen Intel i7-1260P, 16 logical CPUs, about 7.8 GB RAM visible to WSL, Go 1.26.0.
- **What is timed:** one direct call to `Reconcile` using the manager's cached
  client, as in production. Not timed: the workqueue, controller-runtime's
  scheduling, or concurrency. The controller runs one worker by default.
- **Sample:** 7 timed repetitions per cell after 1 discarded warm-up. The table
  shows the median; min and max are in the raw file.
- **What this is not:** a cluster with real network latency, other workloads,
  many ConfigSyncs at once, or a production-sized etcd. There is no throughput
  figure here (reconciles per second across many objects); only per-reconcile
  latency for one object at a time. Nothing here has been measured outside
  envtest, and the absolute numbers will differ on other hardware and clusters.

## Results (median, milliseconds)

Setting: real Event recorder and the development logger, i.e. how `cmd/main.go`
runs the controller.

| Target namespaces (N) | 1 | 10 | 50 | 100 |
|---|---:|---:|---:|---:|
| All exist, first reconcile (creates N ConfigMaps) | 9.4 | 37.5 | 146.5 | 222.4 |
| All exist, steady state (nothing to change) | 7.5 | 6.5 | 7.6 | 11.0 |
| **All missing**, first reconcile | 62.4 | 583.4 | 2887.2 | 5792.9 |
| **All missing**, repeat reconcile | 63.6 | 581.6 | 2878.9 | 5828.6 |

Arithmetic on the table above, not separate measurements: creating costs about
2 ms per namespace at N=100; a missing namespace costs about 58 ms per
namespace, roughly 25 times more. The steady-state no-op is between 6 and 11 ms
at every size, with no clear trend at this resolution.

## Finding: a missing namespace costs about 58 ms per reconcile

Removing logging and the Event recorder did not change it. At N=100 with all
namespaces missing, the median was 5793 ms with the recorder and logging, 5750
ms with logging off, and 5763 ms with a fake recorder and logging off (see the
raw file). So the cost is not in this controller's logging or Events.

The cost comes from the API server. Creating an object in a namespace that does
not exist makes the `NamespaceLifecycle` admission plugin sleep before it
rejects the request. In `k8s.io/apiserver` v0.36.0
(`pkg/admission/plugin/namespace/lifecycle/admission.go`) that is
`missingNamespaceWait = 50 * time.Millisecond`, with the comment "give the cache
time to observe the namespace before rejecting a create". 50 ms plus about 8 ms
of ordinary request time matches the measured 58 ms. That plugin is enabled by
default, so a real cluster should behave the same way, but I have only measured
it in envtest.

## What it means

- One ConfigSync with N missing namespaces holds the (single) worker for about
  `58 ms x N` on every attempt, and failed attempts are retried with backoff.
  At N=100 that is nearly 6 seconds per attempt, during which no other
  ConfigSync is reconciled.
- This is consistent with what I saw earlier: leaving such an object in the
  cluster made the test suite go from roughly 10 s to 24-44 s and time out a
  spec. I did not separately time that run.
- The maximum of 100 target namespaces on `spec.targetNamespaces` bounds this
  at about 6 s per attempt. That cap is an arbitrary guardrail, not derived
  from these numbers.

## Options, not implemented

1. Check that each namespace exists from a cached `Namespace` informer before
   trying to create, and skip missing ones. This avoids the 50 ms wait but adds
   a cluster-wide `namespaces` get/list/watch permission.
2. Raise `MaxConcurrentReconciles` so one slow ConfigSync does not block the
   others. This does not make the slow one faster.
3. Leave it. The cost is bounded by the 100-namespace cap and only affects a
   misconfigured ConfigSync.
