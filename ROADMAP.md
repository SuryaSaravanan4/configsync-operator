# Roadmap

Written against commit `a5ecbb7`; items marked Fixed/Done have since been implemented.
Effort: S (< half a day), M (about a day), L (multi-day). Interview value is a
judgement call about reconciliation correctness, operational safety, and
observability, not a measurement.

## What exists today

- One reconciler (`internal/controller/configsync_controller.go`): create-or-update
  per target namespace, label-based pruning, `Ready` condition, `observedGeneration`.
- CRD validation via kubebuilder markers and one CEL rule (name <= 63 chars).
- envtest suite (24 specs): most call `Reconcile` directly; four run a real manager to prove the watch wiring.
- CI: lint, envtest (`make test`), and a kind e2e that deploys the manager and exercises reconciliation, RBAC-gated Events and cascade GC.
- Emits Events via an `events.k8s.io` recorder. No webhooks, no Secrets handling.

## Gaps found during code review (not in the original list)

### A. Prune can delete a ConfigMap we own after a transient failure (bug)
`Reconcile` passes `synced` to `pruneOrphans` as the keep-list. If an update to a
still-targeted namespace fails (API error, conflict), that namespace is missing
from `synced`, so `pruneOrphans` sees our own labelled, owned ConfigMap there as
an orphan and deletes it. The keep-list should be `spec.targetNamespaces`.
**Fixed in `ef5874b`.** Reproduced first with a failing envtest spec (interceptor
client injecting an Update error in one namespace), then fixed by pruning against
`spec.targetNamespaces`. Envtest only; not exercised on a real cluster.
- Effort: S. Interview value: high (reconciliation correctness, operational safety).

### B. Watch wiring is untested (fixed)
Every spec calls `Reconcile` by hand. Nothing checks that `Owns(&ConfigMap{})`
actually turns a hand-deleted ConfigMap into a reconcile, which is the headline
behaviour in the README. **Fixed:** `configsync_manager_test.go` runs a real manager in envtest (only for that spec group, so it cannot race the direct-`Reconcile` specs) and uses `Eventually`. Mutation-checked: with `Owns()` removed the hand-delete spec fails on timeout. Envtest only.
- Effort: S-M. Interview value: high (reconciliation correctness).

### C. Missing target namespace is untested
A target namespace that does not exist makes `CreateOrUpdate` fail, the CR goes
`Ready=False/SyncFailed`, and the reconcile returns an error (backoff).
**Covered in `ef5874b`**: the spec passed without any code change, so the behaviour
was already correct and is now pinned.
- Effort: S. Interview value: medium.

### D. A missing target namespace costs 58 ms per reconcile (fixed)
Measured in envtest: the API server sleeps 50 ms before rejecting a create into a
namespace it cannot find, so 100 missing namespaces held the single worker for
about 5.8 s per attempt. **Fixed** with a cached Namespace lookup before writing:
about 18 ms at N=100 after (see `docs/measurements.md`). Costs a cluster-wide read
of namespaces. Envtest only; the RBAC rule is exercised by the kind e2e in CI.
- Effort: S. Interview value: high (operational safety, measured not assumed).

## Items from the original list

| # | Gap | Effort | Interview value | Notes |
|---|-----|--------|-----------------|-------|
| 1 | ~~No Kubernetes Events~~ | S | High (observability) | **Done.** `events.EventRecorder` (events.k8s.io) on the reconciler; Events on create, update, prune, conflict, failure; silent on no-op reconciles. `events.k8s.io` create/patch RBAC added. Verified with `FakeRecorder` specs and, in a throwaway envtest run with a real manager, by reading back the real Event objects. The RBAC rule is now verified in the kind e2e (a `Created` Event is recorded under the real ServiceAccount, no `forbidden` in the manager log). Not verified beyond a single-node kind cluster on a CI runner. |
| 2 | ~~Cascade delete is untested~~ | M | High (correctness) | **Done** in the kind e2e: delete the ConfigSync, then `Eventually` its ConfigMaps are gone in both namespaces (the API server GC does it, not the controller). Ran in CI: 6 of 6 e2e specs passed. Single-node kind (`kindest/node:v1.37.0`) only. |
| 3 | ~~e2e does not exercise reconciliation~~ | M | High (operational safety) | **Done** in `test/e2e/e2e_test.go`: applies a ConfigSync to the deployed manager, checks owned ConfigMaps, `Ready=True`, hand-delete recovery, Events under the real ServiceAccount, and GC. Ran in CI on branch `e2e-reconcile`. |
| 4 | No admission webhook | L | Medium | **Partly addressed without a webhook:** CEL rules on the CRD now reject `kube-system`, `kube-public` and `kube-node-lease`, malformed namespace names, and more than 100 namespaces (arbitrary cap, needed to keep the CEL cost within budget). Envtest-verified. A real webhook is still only worth it for checks CEL cannot express (e.g. namespace existence). Original note: Needs webhook server wiring, cert-manager or self-signed certs, kustomize patches, and a webhook e2e. CEL already covers the current rules; a webhook only adds value for checks CEL cannot express (e.g. namespace existence, deny-list such as `kube-system`). A CEL rule or ValidatingAdmissionPolicy would cover a deny-list at S effort. |
| 5 | No Secrets support | M | Medium | Widens RBAC to Secrets cluster-wide, which is a security regression the README currently avoids. Needs a design decision (separate CRD vs. `kind` field) first. |
| 6 | No throughput/latency numbers | M | Low-medium | Only meaningful if measured, and a laptop kind cluster gives numbers that say little about production. If done, report them as "single-node kind on <hardware>, N namespaces" and nothing broader. |

## Constraints on claims

- envtest behaviour is not cluster behaviour: no GC, no namespace lifecycle, no
  admission webhooks unless installed, no kubelet.
- Everything has only run on a local kind cluster. Nothing here is
  production-tested and none of the above changes that.
- No performance numbers exist today.
