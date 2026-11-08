# ClusterServiceAccountSPIFFEID

This opt-in, cluster-scoped `spire.spiffe.io/v1alpha1` resource generates one
SPIRE registration per selected namespace/service-account pair for each rule.
It intentionally registers service accounts that have no running Pods.

Unlike ClusterSPIFFEID, it never reads Pods, Nodes or Endpoints to generate
entries. It watches Namespace and ServiceAccount creation, deletion and metadata
changes, plus changes to the identity rule. It feeds SCM's existing shared
entry reconciliation process rather than using a separate SPIRE writer.
ClusterStaticEntry bootstrap entries can be reconciled in the same process.

## Fields

| Field | Required | Meaning |
|-------|----------|---------|
| `namespaceSelector` | Yes | Kubernetes label selector for enrolled namespaces. `{}` selects all namespaces, subject to `ignoreNamespaces`. |
| `serviceAccountSelector` | No | Kubernetes label selector within selected namespaces. Omitted or `{}` selects all service accounts. |
| `spiffeIDTemplate` | Yes | Nonempty Go text template for the workload SPIFFE ID. |
| `parentIDTemplate` | Yes | Nonempty Go text template for the parent authorized by the SPIRE deployment. |
| `className` | No | Uses SCM's existing `className` and `watchClassless` filtering. |

Both templates expose `TrustDomain`, `ClusterName`, `NamespaceMeta` and
`ServiceAccountMeta`. The metadata fields are Kubernetes `ObjectMeta` values,
so names, labels and annotations are available. There are no Pod, Node or
container fields. Templates use `missingkey=error`; missing map keys in field
expressions are errors. Both rendered IDs must be valid SPIFFE IDs in the
configured trust domain.

Every entry has exactly these selectors, derived from the objects rather than
from templates:

```text
k8s:ns:<namespace>
k8s:sa:<service-account>
```

There is no Pod UID selector and no arbitrary selector override. SVID TTLs use
SPIRE defaults; this resource does not grant admin or downstream privileges.

## Example

```yaml
apiVersion: spire.spiffe.io/v1alpha1
kind: ClusterServiceAccountSPIFFEID
metadata:
  name: cilium-mtls
spec:
  namespaceSelector:
    matchLabels:
      io.cilium/mtls-enabled: "true"
  spiffeIDTemplate: "spiffe://{{ .TrustDomain }}/ns/{{ .NamespaceMeta.Name }}/sa/{{ .ServiceAccountMeta.Name }}"
  parentIDTemplate: "spiffe://{{ .TrustDomain }}/ns/spire/sa/spire-agent"
```

The parent in this example is a shared SPIRE agent alias. Replace its namespace
and service-account name with those used by the deployment. A constant template
such as `spiffe://{{ .TrustDomain }}/ztunnel` is also accepted, but acceptance by
SCM does not prove that the configured agents or delegated identity clients can
obtain those identities. Verify the parent chain and SVID authorization with
the actual SPIRE/Cilium deployment.

Install the generated CRD from
`config/crd/bases/spire.spiffe.io_clusterserviceaccountspiffeids.yaml`. The
manager needs `get/list/watch` on namespaces, service accounts and this
resource, and `get/update/patch` on its status. The standard generated manager
role retains the permissions needed by the legacy controllers. A deployment
that disables them can omit Pod, Node and Endpoints permissions.

Enable the controller explicitly:

```yaml
entryIDPrefix: cilium-mtls-poc
reconcile:
  clusterSPIFFEIDs: false
  clusterServiceAccountSPIFFEIDs: true
  clusterStaticEntries: true
  clusterFederatedTrustDomains: false
```

See the complete [manager example](../examples/service-account-controller.yaml)
and [resource example](../config/samples/spire_v1alpha1_clusterserviceaccountspiffeid.yaml).
The controller is disabled when the new flag is omitted, including in the
default configuration. Static-manifest mode cannot enable this controller.

## Convergence and ownership

Removing namespace enrollment, removing a selected service account, changing
selectors or templates, or deleting a rule removes its no-longer-declared
registrations. Terminating namespaces, service accounts and rules are excluded.
Changes trigger reconciliation; the normal `gcInterval` also repairs missed
events and SPIRE-side drift.

Registration IDs use a stable `serviceaccount.` scope under `entryIDPrefix`,
derived from the trust domain, cluster name and controller class, with an
entry-content identifier. Restarts and ambiguous create responses do not
produce new IDs. Identical declarations from overlapping rules share an entry
using SCM's existing precedence and masking logic. Different SPIFFE IDs or
parents remain distinct declarations even for the same pair.

When only this controller is enabled, it manages only its scoped registrations
and leaves unrelated entries untouched. An identical unmanaged entry is not
silently adopted; SPIRE's duplicate-entry response is reported as a failure.
When legacy or static controllers are
also enabled, their existing whole-prefix reconciliation semantics still apply:
declare all entries they should retain and use a unique `entryIDPrefix` for
each independent manager. Never run two writers for the same prefix. The
service-account and static paths share one desired-state snapshot and cannot
collect each other's valid declarations.

When switching from Pod mode to service-account mode with static reconciliation
still enabled, old per-Pod entries inside the same `entryIDPrefix` have no
remaining declarations and are removed. Registrations outside that prefix are
not part of that migration.

Disabling the new flag leaves its scoped registrations untouched. Remove the
rules or unenroll namespaces while it is still enabled before disabling it or
changing the manager's cluster, class or prefix. This avoids orphaning the old
scope.

## Failures and status

`status.observedGeneration` and the `Ready` condition report the most recent
reconciliation. Statistics count selected/ignored namespaces, selected service
accounts, render failures, masked entries, desired entries and SPIRE write
failures.

Invalid template syntax or selectors produce `InvalidSpec`. Execution errors,
invalid IDs and trust-domain mismatches produce `RenderFailed`. Invalid rules
or failed pairs declare no entries, so their previous registrations are
removed on a successful SPIRE pass. This is intentionally fail-closed.
Kubernetes list failures abort the write pass rather than interpreting an
incomplete snapshot as unenrollment. SPIRE API failures are logged and reported
as `SPIREAPIError`; periodic reconciliation retries them. Status write failures
are logged and retried on subsequent reconciliations. A deleted rule cannot
carry status, so deletion failures are reported in logs.

`Ready=True` means SCM reconciled desired registrations, not that any workload
successfully obtained an SVID or established an mTLS connection.

## Identity and security tradeoff

All workloads using the same namespace and service-account name share this
identity, across Pods and Nodes allowed by the parent relationship. Pod UID and
node-specific binding are intentionally absent. Pod churn therefore does not
create registrations or require controller work. Cost scales with selected
namespaces and service accounts, not the number of Pods.

Deleting and recreating a service account under the same name reuses its
name-based identity. Removing a registration prevents subsequent issuance once
SPIRE caches converge; it does not instantly revoke already-issued SVIDs.
Workloads must still pass SPIRE workload attestation or the deployment's
delegated identity authorization. This resource does not implement that
authorization.

Treat namespace enrollment, service-account selection labels, template inputs,
the chosen parent and the ability to create workloads under a service account
as identity-authorizing configuration. Restrict their mutation through
Kubernetes RBAC and validate the shared parent/delegation trust boundary before
using this model outside an isolated POC.

## Local development

The generator is pinned to controller-tools v0.17.3 to match the Kubernetes
v0.32 APIs. Generation is limited to the API and controller packages, not the
separate demo module. For reproducible generation using the repository's Go
version:

```sh
GOMAXPROCS=2 GOFLAGS="-p=2" GOTOOLCHAIN=go1.23.4 make generate manifests
GOMAXPROCS=2 GOFLAGS="-p=2" make envtest
bin/setup-envtest use 1.28.0 --bin-dir bin -p path
GOMAXPROCS=2 go test -p 2 ./...
GOMAXPROCS=2 go test -race -p 2 ./pkg/spireentry ./cmd -run TestServiceAccount
```

The tests include a real local API server/informer check that rejects any
manager request for Pods, Nodes or Endpoints with the legacy controller
disabled, plus shared-store lifecycle tests that retain four static bootstrap
entries while service-account entries change.

Build locally without publishing:

```sh
docker build --platform linux/amd64 --target spire-controller-manager \
  --build-arg goversion=1.23.4 --build-arg GOMAXPROCS=2 \
  --build-arg GOFLAGS=-p=2 -t spire-controller-manager:service-account-poc .
```
