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

## Results before the pre-check (median, milliseconds)

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

## What it meant before the pre-check

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

## After the namespace pre-check

The first option from the earlier version of this document was then implemented: `syncNamespace` looks the namespace up (from
the cache in production) before writing to it and skips the create when it is
missing. The failure is still reported and the reconcile is still retried with
backoff, so behaviour is unchanged apart from the API call not being made. Same
harness, same conditions, same setting as the table above. Raw output:
[measurements-raw-after-precheck.txt](measurements-raw-after-precheck.txt).

| Target namespaces (N) | 1 | 10 | 50 | 100 |
|---|---:|---:|---:|---:|
| All exist, first reconcile (creates N ConfigMaps) | 6.8 | 31.3 | 161.2 | 341.5 |
| All exist, steady state (nothing to change) | 4.5 | 6.1 | 8.8 | 11.4 |
| **All missing**, first reconcile | 4.7 | 5.1 | 10.6 | 18.4 |
| **All missing**, repeat reconcile | 5.5 | 6.3 | 9.4 | 23.4 |

- **Missing namespaces:** at N=100 the median went from 5793 ms to 18.4 ms
  (first reconcile) and from 5829 ms to 23.4 ms (repeat), about 300 times less
  (arithmetic on the two tables). The cost no longer grows at 58 ms per
  namespace.
- **Steady state:** 11.0 ms before, 11.4 ms after at N=100.
- **Creating at N=100: no conclusion.** The medians were 222, 270 and 290 ms
  before and 342, 290 and 235 ms after, across the three logging/recorder
  settings, which should behave the same. Run-to-run spread is as large as the
  difference, so these numbers cannot show whether creating got slower or not.
  A cached namespace lookup per namespace is tiny next to a 2 ms API write, but
  I have not measured it separately.

Costs of the change that these numbers do not capture:

- The manager now needs cluster-wide `get`/`list`/`watch` on `namespaces`, and
  runs a Namespace informer. That is a wider permission than before.
- A namespace created just after a ConfigSync may not be in the cache yet. It is
  then reported as missing and picked up by the backoff retry, which starts at a
  few milliseconds. I have not measured how often that happens.
- The Namespace watch described in the next section closes this gap: a
  namespace created after the ConfigSync now triggers a reconcile directly
  instead of waiting for backoff.

## Namespace watch

The manager also watches Namespace creation and enqueues every ConfigSync that
lists the new namespace. Before this, a ConfigSync waiting on a missing
namespace was retried only on exponential backoff. The spec for it
(`configsync_manager_test.go`) lets the backoff grow for 6 seconds, when the
next retry is several seconds away, then creates the namespace and requires the
ConfigMap within 2 seconds. Before the watch existed that spec timed out after
2.001 s; with it, it passes. That is a pass/fail check of behaviour, not a
latency measurement, and I did not time how quickly the ConfigMap appears.

Not measured: the map function scans the cached ConfigSync list linearly for
each namespace event, and every existing namespace produces one event when the
informer first syncs, so a manager start costs roughly namespaces x ConfigSyncs
list scans. That is cheap for small clusters and unmeasured for large ones.

## Remaining options

2. Raise `MaxConcurrentReconciles` so one slow ConfigSync does not block the
   others. Not implemented: there is no spec yet showing that the blocking
   actually happens, so it would be a guess.
