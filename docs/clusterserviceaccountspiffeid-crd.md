# ClusterServiceAccountSPIFFEID

This opt-in, cluster-scoped `spire.spiffe.io/v1alpha1` resource evaluates
selected namespace/service-account pairs for each rule. By default, it generates
one SPIRE registration per pair. Optional selector templates allow other
workload selectors without changing the objects it enumerates. Service accounts
with no running Pods are intentionally included.

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
| `workloadSelectorTemplates` | No | Nonempty list of templates producing the complete SPIRE selector set. Omitted or null retains the default namespace/service-account selectors. `[]` is invalid. |
| `className` | No | Uses SCM's existing `className` and `watchClassless` filtering. |

All templates expose `TrustDomain`, `ClusterName`, `NamespaceMeta` and
`ServiceAccountMeta`. The metadata fields are Kubernetes `ObjectMeta` values,
so names, labels and annotations are available. There are no Pod, Node or
container fields, including no synthetic `PodMeta` or `PodSpec`. Go
`text/template` built-in functions such as `index` and `printf` are available;
there are no extra template functions. Templates use `missingkey=error`;
missing map keys in field expressions are errors. The built-in `index` keeps
Go's normal behavior of returning a zero value for a missing map key, so
templates using it must handle absent metadata deliberately. Both rendered IDs
must be valid SPIFFE IDs in the configured trust domain.

When `workloadSelectorTemplates` is omitted, every entry has exactly these
selectors derived from the objects:

```text
k8s:ns:<namespace>
k8s:sa:<service-account>
```

An explicit list replaces the entire set. No namespace, service-account or
Pod UID selector is implicitly appended. Each selector is split on its first
colon into `type:value`; further colons and payload whitespace are preserved.
The type and value must be nonempty and not whitespace-only. The selector list
is sorted by type then value and exact duplicates are removed. A failed
template or invalid selector rejects the entire candidate entry, never just
that selector.

SPIRE treats the selector set as constraints that must all match, not as
alternatives. SCM validates the selector framing but cannot establish whether
a particular workload attestor supports or emits a selector. SVID TTLs use
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
  workloadSelectorTemplates:
    - "k8s:ns:{{ .NamespaceMeta.Name }}"
    - "k8s:sa:{{ .ServiceAccountMeta.Name }}"
```

These explicit templates are equivalent to omitting the field, including
registration IDs. Reordering or repeating selectors does not change the
resulting selector set or registration ID. To add a constraint derived from
service-account metadata, explicitly retain both bindings:

```yaml
workloadSelectorTemplates:
  - "k8s:ns:{{ .NamespaceMeta.Name }}"
  - "k8s:sa:{{ .ServiceAccountMeta.Name }}"
  - "k8s:pod-label:app:{{ .ServiceAccountMeta.Labels.app }}"
```

The last selector is only a declaration of what SPIRE should attest. It does
not make SCM read or watch Pods. Validate the selector spelling and issuance
behavior against the installed SPIRE attestor and delegated identity path.
Arbitrary types, metadata keys and colon-containing values are supported, for
example `custom:resource:{{ index .ServiceAccountMeta.Annotations "example.org/resource" }}`.

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
produce new IDs. The baseline namespace/service-account ID algorithm is
preserved for that selector shape. Other selector sets use an unambiguous
content digest so selector field boundaries cannot conflate distinct entries.
Shared-store grouping also preserves those boundaries; existing entries keep
their stored registration IDs when reused.

Candidates within one rule that render the same SPIFFE ID, parent ID and
selector set coalesce into one registration. For example, an identity template
`spiffe://{{ .TrustDomain }}/ns/{{ .NamespaceMeta.Name }}` with only
`k8s:ns:{{ .NamespaceMeta.Name }}` produces one entry for a namespace with at
least one selected service account. Removing one account leaves the entry
while another selected account still declares it; removing the last candidate
collects it. A constant identity and selector set can similarly coalesce across
namespaces.

Identical declarations from different rules share an entry using SCM's
existing precedence and masking logic. Different SPIFFE IDs or parents remain
distinct declarations even when their selectors match. Candidate enumeration
order and selector ordering do not affect these identities.

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
failures. `serviceAccountsSelected` counts candidate objects;
`entriesToSet` counts unique preferred entries. Coalesced candidates in the
same rule do not increase `entriesMasked`, which counts masking by other rules.

Invalid template syntax, Kubernetes label selectors or an explicitly empty
template list produce `InvalidSpec`. Execution errors, malformed rendered
selectors, invalid IDs and trust-domain mismatches produce `RenderFailed`.
The CRD schema also rejects an explicitly empty list and empty template strings.
Invalid rules
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

With the default selectors, all workloads using the same namespace and
service-account name share this identity, across Pods and Nodes allowed by the
parent relationship. Pod UID and node-specific binding are intentionally
absent. Pod churn therefore does not create registrations or require controller
work. Cost scales with selected namespaces and service accounts, not the number
of Pods, even when selector templates mention properties SPIRE attests on Pods.

Namespace/service-account selection controls which objects generate
declarations; it is **not an authorization selector**. If an explicit selector
list omits namespace or service-account bindings, workloads outside those
selected objects may qualify under the chosen parent. An identity path that
contains namespace or service-account names does not add those bindings.
For example, `unix:uid:1000` alone may match any workload with that attested
UID authorized by the parent, regardless of the candidate's namespace.
Likewise, copying a label or annotation into a selector does not itself prove
that the workload has that attribute. Retain the ns/sa selectors when that
boundary is required.

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
entries while service-account entries change. Selector-template tests also
cover omitted and explicit defaults, baseline ID stability, colon preservation,
metadata changes, fail-closed errors, duplicate candidates, selector ordering
and unambiguous entry keys. The API-server test checks the explicit list,
null/omitted behavior, empty-list rejection and annotation-driven events.

Build locally without publishing:

```sh
docker build --platform linux/amd64 --target spire-controller-manager \
  --build-arg goversion=1.23.4 --build-arg GOMAXPROCS=2 \
  --build-arg GOFLAGS=-p=2 -t spire-controller-manager:service-account-poc .
```
