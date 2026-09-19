# configsync-operator

A Kubernetes operator that keeps a ConfigMap materialized across a set of
namespaces, and puts it back if someone deletes it by hand.

I built this as a learning project to get past reading about controller-runtime
and actually write one: a CRD, a reconciler, envtest coverage, and a `kind`
cluster to poke at by hand.

## Why

The recurring pattern this solves: you have one config payload (a log level, a
feature flag set, some shared value) that needs to exist identically in
several namespaces, and you want it to *stay* that way even if someone
`kubectl delete`s the ConfigMap in one of them, or if the list of namespaces
that need it changes over time. `ConfigSync` is a single CRD you point at a
list of target namespaces and a data map; the controller keeps a ConfigMap
matching that spec in every one of them, recreates it if it goes missing, and
removes it from namespaces you drop from the list.

The reconciler is level-triggered, the way controller-runtime expects: it
doesn't care *why* it was invoked, only what the current spec says. Every call
re-derives the desired state and reconciles the cluster toward it, whether
that means creating a ConfigMap that's missing, overwriting one that's
drifted, or deleting one that's no longer wanted.

## CRD spec

`ConfigSync` is cluster-scoped. `spec.targetNamespaces` needs at least one
entry, `spec.data` needs at least one key.

```yaml
apiVersion: platform.saravanan.dev/v1alpha1
kind: ConfigSync
metadata:
  name: configsync-sample
spec:
  targetNamespaces:
    - team-a
    - team-b
  data:
    LOG_LEVEL: info
    FEATURE_FLAGS: "beta=true"
```

Applying this creates a ConfigMap named `configsync-sample` in both `team-a`
and `team-b`, each carrying `spec.data` verbatim plus two tracking labels
(`platform.saravanan.dev/managed-by`, `platform.saravanan.dev/configsync`) and
a controller ownerRef back to the ConfigSync. Drop `team-b` from the list and
the next reconcile deletes the ConfigMap there; delete the ConfigMap in
`team-a` by hand and the next reconcile puts it back.

`status.conditions` carries a `Ready` condition: `True` once every target
namespace is converged, `False` if a namespace already has a ConfigMap by
that name that this controller didn't create — the controller refuses to
adopt or overwrite a ConfigMap it doesn't own. `status.syncedNamespaces` lists
the namespaces currently converged, and `status.observedGeneration` tracks
which version of the spec that status reflects.

## Running it locally against `kind`

```sh
# 1. Spin up a local cluster
kind create cluster

# 2. Install the CRD
make install

# 3. Run the controller in the foreground (not deployed as a Pod)
make run
```

With that running, in another terminal:

```sh
# Create the namespaces the sample targets
kubectl create namespace team-a
kubectl create namespace team-b

# Apply the sample ConfigSync
kubectl apply -k config/samples/

# Confirm the ConfigMap landed in both namespaces
kubectl get configmap configsync-sample -n team-a -o yaml
kubectl get configmap configsync-sample -n team-b -o yaml

# Check status
kubectl get configsync configsync-sample -o yaml
```

To tear down: `kubectl delete -k config/samples/`, `make uninstall`, then
`kind delete cluster`.

## What's verified

Unit/integration coverage runs against envtest (real etcd + kube-apiserver,
no controller-manager) and covers:

- Creating the ConfigMap in every target namespace with the right data,
  labels, and ownerRef
- A second reconcile of unchanged state writing nothing (no self-sustaining
  reconcile loop from `Owns()` feeding our own writes back to us)
- Recreating a ConfigMap that was deleted out from under the controller
- Converging a drifted ConfigMap back to spec, including removing keys no
  longer present
- Pruning a ConfigMap from a namespace dropped from `targetNamespaces`
- Refusing to adopt or overwrite a ConfigMap that already exists but wasn't
  created by this controller
- Status conditions and `observedGeneration` tracking
- Watch wiring: with a real manager running in envtest, creating a ConfigSync,
  hand-deleting a ConfigMap, hand-editing one, and changing the spec each
  converge without any manual `Reconcile` call
- Kubernetes Events on create, update, prune, conflict and failure, and none on a
  no-op reconcile (asserted with a fake recorder)

Cascade deletion of ConfigMaps when the owning `ConfigSync` itself is deleted
relies on Kubernetes' garbage collector reading the ownerRef. envtest doesn't
run a controller-manager, so that path is covered by the `kind` e2e job instead
(see below), not by the envtest suite.

## Tech stack

Go, kubebuilder (scaffolding + CRD/RBAC generation via markers), client-go,
controller-runtime. CI (GitHub Actions) runs three jobs on every push: golangci-lint,
the envtest suite above, and a `kind`-based e2e job. The e2e job deploys the
manager as a Pod under its generated ServiceAccount and ClusterRole, checks that
it serves its metrics endpoint, and then applies a `ConfigSync` to check that
ConfigMaps are created and owned, that `Ready` becomes `True`, that a
hand-deleted ConfigMap is recreated, that an Event is recorded (which exercises
the `events.k8s.io` RBAC rule), and that deleting the `ConfigSync`
garbage-collects its ConfigMaps. That is a single-node `kind` cluster on a CI
runner; it is a real API server and garbage collector, not a production cluster.

## Known limitations / non-goals

- **Cluster-scoped only.** There's no per-namespace RBAC story here — anyone
  who can create a `ConfigSync` can write a ConfigMap into any namespace it
  lists, except `kube-system`, `kube-public` and `kube-node-lease`, which the
  CRD rejects. That deny-list is a schema rule, not access control: it does not
  stop the same person targeting any other sensitive namespace.
- **No Secrets support.** Only ConfigMaps; syncing Secrets would need separate
  RBAC and probably shouldn't share this exact controller.
- **No webhooks.** Validation is CEL and kubebuilder markers on the CRD, not an
  admission webhook: at least 1 and at most 100 target namespaces (the cap is an
  arbitrary guardrail, not a measured limit), each a valid DNS label, no
  control-plane namespaces, name length, non-empty data. There's no defaulting.
- **Conflict handling is a name conflict, not a merge.** If a ConfigMap with
  the same name already exists in a target namespace and isn't owned by this
  controller, that namespace is skipped and reported via the `Ready`
  condition — it is never adopted or overwritten.
- **Not load-tested.** This was built and exercised against a single-node
  `kind` cluster with a handful of namespaces, not a production-scale cluster.
- **Learning project, never deployed for real.** The point of this repo was to
  understand Go and the controller-runtime reconciliation model by writing a
  controller end to end rather than reading about one. It has only ever run
  against a local `kind` cluster — usually via `make run` from a laptop rather
  than as a deployed Pod — and has never been installed in a shared, staging,
  or production cluster. Treat it as a study project, not a tool to adopt.
