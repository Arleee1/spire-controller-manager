# ClusterSPIFFEIDSet

`spire.spiffe.io/v1alpha1 ClusterSPIFFEIDSet` is an opt-in cluster-scoped
identity rule over **explicit Kubernetes inputs**. It does not infer objects
from SPIRE selector strings, add selectors, or enumerate Pods.

Unlike ClusterServiceAccountSPIFFEID, the iteration domain is configurable.
Namespace-only rules never enumerate service accounts, service-account-only
rules never enumerate namespaces, and a constant rule has no workload inputs.
Other administrator-allowed kinds, including custom resources, use the same
unstructured input path.

## API

| Field | Required | Meaning |
|-------|----------|---------|
| `className` | No | Existing SCM controller-class filtering. |
| `forEach` | No | Ordered object bindings. Omitted or `[]` yields one constant candidate. |
| `spiffeIDTemplate` | Yes | Nonempty Go text template for a valid identity in this manager's trust domain. |
| `parentIDTemplate` | Yes | Nonempty parent identity template in the same trust domain. |
| `workloadSelectorTemplates` | Yes | Nonempty list rendering the entire SPIRE selector set. No implicit ns, sa or Pod UID selector. |

Each `forEach` item has:

| Field | Required | Meaning |
|-------|----------|---------|
| `alias` | Yes | Unique identifier matching `[A-Za-z][A-Za-z0-9_]*`. |
| `apiVersion` | Yes | Exact Kubernetes API version, such as `v1` or `example.org/v1`. |
| `kind` | Yes | Exact object kind, not a plural resource name, list kind or subresource. |
| `labelSelector` | No | Kubernetes label selector. Omission matches all objects of this input kind. |
| `namespaceTemplate` | No | For namespaced kinds, restrict to one namespace using globals and earlier bindings. Omission enumerates all namespaces without fetching Namespace objects. |

A namespace template must render a nonempty valid namespace name. It cannot
be used for a cluster-scoped input. Later bindings do not exist yet; references
to unavailable fields fail rendering rather than causing other object reads.
`ignoreNamespaces` is applied to namespaced objects' metadata and to selected
Namespace names, without additional Namespace lookups. Terminating input
objects and terminating rules are excluded.

## Template context

The context is `TrustDomain`, `ClusterName`, and `Inputs`. Each
`Inputs.<alias>` is the complete unstructured Kubernetes object with ordinary
JSON keys:

```text
.Inputs.ns.metadata.name
.Inputs.sa.metadata.namespace
.Inputs.sa.metadata.labels.app
.Inputs.config.data.identity
.Inputs.widget.spec.selector
```

Go `text/template` built-in functions are available, including `index` for
metadata keys containing punctuation. There are no synthetic Pod, Node or
service-account fields. Field access uses `missingkey=error`; Go's built-in
`index` retains its ordinary zero-value behavior for a missing map key.

Each selector is parsed at its first colon into a nonblank type and value.
Additional colons and payload whitespace are preserved. Selector order is
canonicalized and exact duplicates are removed. A failed or invalid selector
rejects the whole candidate, never just that constraint.

## Examples

The following are alternative `spec` values, not resources to install
unconditionally.

### Constant registration, no workload inputs

```yaml
spiffeIDTemplate: "spiffe://{{ .TrustDomain }}/shared"
parentIDTemplate: "spiffe://{{ .TrustDomain }}/parent"
workloadSelectorTemplates:
  - "custom:role:shared"
```

### Namespace only

```yaml
forEach:
  - alias: ns
    apiVersion: v1
    kind: Namespace
    labelSelector:
      matchLabels:
        io.cilium/mtls-enabled: "true"
spiffeIDTemplate: "spiffe://{{ .TrustDomain }}/ns/{{ .Inputs.ns.metadata.name }}"
parentIDTemplate: "spiffe://{{ .TrustDomain }}/parent"
workloadSelectorTemplates:
  - "k8s:ns:{{ .Inputs.ns.metadata.name }}"
```

There is one candidate per selected namespace, including namespaces with zero
service accounts. No ServiceAccount informer is started.

### Service account only

```yaml
forEach:
  - alias: sa
    apiVersion: v1
    kind: ServiceAccount
    labelSelector:
      matchLabels:
        identity-enabled: "true"
spiffeIDTemplate: "spiffe://{{ .TrustDomain }}/ns/{{ .Inputs.sa.metadata.namespace }}/sa/{{ .Inputs.sa.metadata.name }}"
parentIDTemplate: "spiffe://{{ .TrustDomain }}/parent"
workloadSelectorTemplates:
  - "k8s:ns:{{ .Inputs.sa.metadata.namespace }}"
  - "k8s:sa:{{ .Inputs.sa.metadata.name }}"
```

Namespace metadata here comes from the ServiceAccount object's own namespace
field, not a Namespace lookup. Removing the ns selector would be supported,
but would weaken the namespace authorization boundary.

### Correlated Namespace to ServiceAccount

```yaml
forEach:
  - alias: ns
    apiVersion: v1
    kind: Namespace
    labelSelector:
      matchLabels:
        io.cilium/mtls-enabled: "true"
  - alias: sa
    apiVersion: v1
    kind: ServiceAccount
    namespaceTemplate: "{{ .Inputs.ns.metadata.name }}"
spiffeIDTemplate: "spiffe://{{ .TrustDomain }}/ns/{{ .Inputs.ns.metadata.name }}/sa/{{ .Inputs.sa.metadata.name }}"
parentIDTemplate: "spiffe://{{ .TrustDomain }}/ns/spire/sa/spire-agent"
workloadSelectorTemplates:
  - "k8s:ns:{{ .Inputs.ns.metadata.name }}"
  - "k8s:sa:{{ .Inputs.sa.metadata.name }}"
```

The downstream binding queries the ServiceAccount cache's namespace index,
not all accounts for every selected namespace. This reproduces the previous
POC identity, parent and selector tuples when the deployment's parent matches.
See the complete [resource sample](../config/samples/spire_v1alpha1_clusterspiffeidset.yaml).

### ConfigMap input

```yaml
forEach:
  - alias: config
    apiVersion: v1
    kind: ConfigMap
    namespaceTemplate: "identity-config"
    labelSelector:
      matchLabels:
        identity-rule: "true"
spiffeIDTemplate: "spiffe://{{ .TrustDomain }}/{{ .Inputs.config.data.identity }}"
parentIDTemplate: "spiffe://{{ .TrustDomain }}/parent"
workloadSelectorTemplates:
  - "{{ .Inputs.config.data.selector }}"
```

Only ConfigMaps are input objects. Neither Namespace nor ServiceAccount
objects are fetched. A namespaced custom resource works identically by using
its exact group/version/kind and fields such as `.Inputs.workload.spec.selector`.

## Iteration, coalescing and watches

`forEach` is evaluated in order. Inputs without a correlation template
explicitly form independent Cartesian combinations. Evaluation streams those
combinations rather than materializing a Cartesian table; administrators
should still avoid unnecessarily large products.

Each active declared GVK has one unstructured list/watch cache. Allowlisting a
kind does not start its informer. No input kind is inferred from selectors.
Deleting the last referencing rule, changing its class, or changing the input
kind retires and stops the unused informer. Unsupported replacement kinds do
not leave the old, no-longer-declared watcher running. Cache shutdown waits for
the input goroutines to stop.

Identical rendered SPIFFE ID, parent and selector sets coalesce within a rule
and across rules. Candidate order and alias spelling do not enter the
registration ID. A coalesced entry remains until no valid candidate declares
it. Across rules, existing SCM creation-time/UID preference determines which
rule owns the shared declaration and which reports it as masked.

## Configuration and permissions

```yaml
reconcile:
  clusterSPIFFEIDs: false
  clusterServiceAccountSPIFFEIDs: false
  clusterSPIFFEIDSets: true
  clusterStaticEntries: true
  clusterFederatedTrustDomains: false
clusterSPIFFEIDSet:
  allowedInputKinds:
    - apiVersion: v1
      kind: Namespace
    - apiVersion: v1
      kind: ServiceAccount
```

The new reconcile flag defaults to false. An empty `allowedInputKinds` permits
only constant rules. The allowlist is exact, with no wildcard interpretation.
Core Pod and Secret inputs are forbidden even if explicitly allowlisted.
Subresources and list kinds are not valid inputs. Static-manifest mode cannot
enable this controller.

The allowlist **does not grant RBAC**. An administrator or Helm deployment must
separately grant read-only permissions on the exact API resources it intends
to expose. The controller never creates roles, grants wildcard access, or
widens permissions after seeing a CR. For custom kinds, the administrator must
know the API group's plural resource name as well as the kind/version.

Input informers use cluster-scope list/watch, with selection and namespace
joins applied to the local indexed cache. A namespace-specific template does
not reduce the API-level RBAC or cache boundary to that namespace. If only
namespace-scoped read permission exists, the cluster-wide list fails
explicitly. Do not configure arbitrary sensitive resources simply because
discovery can find them.

The legacy `cacheNamespace`/`cacheNamespaces` configuration is not supported
with generic reconciliation and is rejected at startup rather than silently
ignored. Express generic input selection through `forEach` instead. This does
not change the existing controllers' cache configuration behavior.

The generated manager role adds only ClusterSPIFFEIDSet and its status
permissions; existing legacy permissions are unchanged. The
[minimal example role](../examples/identity-set-rbac.yaml) covers the joined
sample and static bootstrap entries when webhooks and other controllers are
disabled. The [manager example](../examples/identity-set-controller.yaml)
does not mutate the allowlist based on resource contents.

## Shared reconciliation and migration

Generic, service-account, legacy Pod and static declarations feed the same
desired-entry store and single SPIRE write pass. Valid static bootstrap
entries are not collected during input/rule changes.

Generic and service-account sources intentionally share the established
scoped content-ID namespace. Its stored `serviceaccount.` prefix is retained
for compatibility, not a separate ownership boundary. Either enabled source
manages that scope. Equal entry tuples reuse existing registration IDs,
including the original ns/sa ID algorithm. Both flags disabled retain scoped
entries, as before. Independent managers must still use exclusive
`entryIDPrefix` values.

For a gap-free SA-to-Set migration:

1. Enable both source controllers with narrow allowed kinds and required RBAC.
2. Create the equivalent generic joined-input rule. Verify the same stored
   entry IDs/tuples and bootstrap entries. If the older SA rule is preferred,
   the Set reports `Ready=True`, zero `entriesToSet`, and masked entries.
3. Delete the old SA rule only after verifying the equivalent generic
   declaration. Its entries transfer within the shared pass without
   create/delete operations.
4. Disable SA reconciliation and restart. The generic controller keeps the
   same scoped entries and only its declared input watches.

The reverse handoff enables both, creates a compatible SA rule, verifies
equivalence, deletes the Set rule, then disables Set reconciliation and
restarts. An old binary cannot interpret the new resource. Restore compatible
old declarations while the new binary still runs before rolling the image
back. Custom non-SA semantics cannot be preserved by simply selecting the old
SA binary.

## Status and failures

`status.observedGeneration` and the `Ready` condition identify the observed
rule generation. `stats.combinations` counts complete candidate bindings,
including the single empty binding for constant rules. `entriesToSet` counts
unique preferred entries; `entriesMasked` counts declarations masked by other
rules. Render and SPIRE write failures have separate counters.

Malformed specs, invalid aliases, forbidden inputs or a namespace template on
a cluster-scoped kind report `InvalidSpec`. Execution/selector/namespace/ID
errors report `RenderFailed`. Invalid rules and failed candidates declare no
entries, so no-longer-declared registrations are collected on a successful
pass.

Discovery, allowlist, list/watch and unsynchronized-cache failures report
`InputUnavailable` and abort the write pass. Existing registrations are
retained rather than treating an incomplete input snapshot as empty. A live
watch error is not hidden behind the last cached list. Recovery retries on
events and `gcInterval`. SPIRE failures report `SPIREAPIError`. Status write
failures and deletion errors after a CR is gone are logged.

`Ready=True` describes registration reconciliation, not successful SVID
issuance, and a fully masked rule can be Ready. Inspect stored tuples and the
preferred rule's status when validating a handoff.

## Authorization boundary

Object selection and combinations determine declaration inputs, not workload
authorization. Only the rendered selectors plus SPIRE parent/attestation
rules constrain issuance. Removing namespace/service-account selectors can
authorize workloads outside the selected input objects. Identity path names
do not add those bindings. Metadata or ConfigMap content used in templates
is security-sensitive identity configuration; restrict its mutation.

This controller does not prove a workload attestor emits an arbitrary
selector. Verify custom selector issuance and rejection against the deployed
SPIRE and delegated-identity path. Removing registrations does not instantly
revoke previously issued SVIDs.

## Validation

The tests cover constants, Namespace-only, ServiceAccount-only, correlated and
independent combinations, ConfigMaps and an envtest custom-resource input.
An explicit two-team by two-environment ConfigMap test covers four independent
tuples from two differently filtered aliases sharing one GVK informer,
reordered inputs/selectors, coalescing duplicate candidates and source deletion.
Real manager request accounting proves excluded kinds are not listed or
watched. Transition tests verify old informer cancellation and last-rule
retirement. Real RBAC denial preserves existing entries and recovers only after
the test administrator explicitly grants the missing permission. Shared-store
tests preserve all six workload and four bootstrap entry IDs across forward
and reverse handoffs, including disabling/restarting each source.
