# ClusterSPIFFEIDSet

`spire.spiffe.io/v1alpha1 ClusterSPIFFEIDSet` is an opt-in, cluster-scoped
declaration of SPIRE registrations over explicit Kubernetes inputs.

Some integrations, including Cilium's namespace/service-account identity
model, need registrations at namespace/service-account granularity rather
than per Pod. Avoiding redundant per-Pod registrations is the primary
benefit for that model. Avoiding Pod enumeration is an additional efficiency
benefit. The API is general so the same controller can support other identity
granularities without introducing a separate resource for each integration.

Existing ClusterSPIFFEID, ClusterStaticEntry and federation behavior remains
the default. Sets feed the existing shared desired-entry store and SPIRE
write pass; they are not a separate registration service.

## API

| Field | Required | Meaning |
|-------|----------|---------|
| `className` | No | Existing controller-class filtering. |
| `forEach` | No | Ordered object bindings. Omitted or `[]` yields one constant candidate. |
| `spiffeIDTemplate` | Yes | Nonempty Go text template for a valid identity in this manager's trust domain. |
| `parentIDTemplate` | Yes | Nonempty parent identity template in the same trust domain. |
| `workloadSelectorTemplates` | Yes | Nonempty list rendering the complete SPIRE selector set. No implicit namespace, service-account or Pod UID selector. |

Each `forEach` item has:

| Field | Required | Meaning |
|-------|----------|---------|
| `alias` | Yes | Unique identifier matching `[A-Za-z][A-Za-z0-9_]*`. |
| `apiVersion` | Yes | Exact Kubernetes API version, such as `v1` or `example.org/v1`. |
| `kind` | Yes | Exact object kind, not a plural resource name, list kind or subresource. |
| `labelSelector` | No | Kubernetes label selector. Omission matches all objects of this input kind. |
| `namespaceTemplate` | No | For namespaced kinds, restrict to one namespace using globals and earlier bindings. Omission selects all namespaces without fetching Namespace objects. |

A namespace template must render a nonempty valid namespace name. It cannot
be used for a cluster-scoped input. Later bindings do not exist yet; references
to unavailable fields fail rendering rather than causing other object reads.
`ignoreNamespaces` applies to input objects' namespaces and to selected
Namespace names, without additional Namespace lookups. Terminating input
objects and terminating rules are excluded.

## Template context

The context contains `TrustDomain`, `ClusterName`, and `Inputs`. Each
`Inputs.<alias>` is the full unstructured object with ordinary JSON keys:

```text
.Inputs.ns.metadata.name
.Inputs.sa.metadata.namespace
.Inputs.sa.metadata.labels.app
.Inputs.config.data.identity
.Inputs.widget.spec.selector
```

Go `text/template` built-in functions are available, including `index` for
metadata keys containing punctuation. No synthetic Pod, Node or service-account
context is inserted. Field access uses `missingkey=error`; Go's built-in `index`
retains its ordinary zero-value behavior for a missing map key.

Each selector is parsed at its first colon into a nonblank type and value.
Additional colons and payload whitespace are preserved. Selector order is
canonicalized and exact duplicates are removed. A failed selector rejects the
whole candidate, never just that constraint.

## Examples

These are alternative `spec` values. The rendered parent must identify an
appropriate existing SPIRE agent or workload parent; Sets do not create or
attest that parent automatically.

### Constant registration

```yaml
spiffeIDTemplate: "spiffe://{{ .TrustDomain }}/shared"
parentIDTemplate: "spiffe://{{ .TrustDomain }}/workload-parent"
workloadSelectorTemplates:
  - "custom:role:shared"
```

No workload-source informer is started, even if the allowlist contains kinds.

### Namespace only

```yaml
forEach:
  - alias: ns
    apiVersion: v1
    kind: Namespace
    labelSelector:
      matchLabels:
        identity-enabled: "true"
spiffeIDTemplate: "spiffe://{{ .TrustDomain }}/ns/{{ .Inputs.ns.metadata.name }}"
parentIDTemplate: "spiffe://{{ .TrustDomain }}/workload-parent"
workloadSelectorTemplates:
  - "k8s:ns:{{ .Inputs.ns.metadata.name }}"
```

There is one candidate per selected namespace, including namespaces with no
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
parentIDTemplate: "spiffe://{{ .TrustDomain }}/workload-parent"
workloadSelectorTemplates:
  - "k8s:ns:{{ .Inputs.sa.metadata.namespace }}"
  - "k8s:sa:{{ .Inputs.sa.metadata.name }}"
```

The namespace comes from the ServiceAccount object's own metadata, not a
Namespace lookup. Both selectors are explicit authorization constraints.

### Correlated Namespace to ServiceAccount

```yaml
forEach:
  - alias: ns
    apiVersion: v1
    kind: Namespace
    labelSelector:
      matchLabels:
        identity-enabled: "true"
  - alias: sa
    apiVersion: v1
    kind: ServiceAccount
    namespaceTemplate: "{{ .Inputs.ns.metadata.name }}"
spiffeIDTemplate: "spiffe://{{ .TrustDomain }}/ns/{{ .Inputs.ns.metadata.name }}/sa/{{ .Inputs.sa.metadata.name }}"
parentIDTemplate: "spiffe://{{ .TrustDomain }}/workload-parent"
workloadSelectorTemplates:
  - "k8s:ns:{{ .Inputs.ns.metadata.name }}"
  - "k8s:sa:{{ .Inputs.sa.metadata.name }}"
```

The downstream binding queries the ServiceAccount cache's namespace index,
not all accounts for each selected namespace. See the complete
[resource sample](../config/samples/spire_v1alpha1_clusterspiffeidset.yaml).

### ConfigMap or custom-resource input

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
parentIDTemplate: "spiffe://{{ .TrustDomain }}/workload-parent"
workloadSelectorTemplates:
  - "{{ .Inputs.config.data.selector }}"
```

Only ConfigMaps are input objects. Neither Namespace nor ServiceAccount
objects are fetched. A namespaced custom resource works identically with its
exact group/version/kind and fields such as `.Inputs.workload.spec.selector`.

## Iteration and watches

Inputs are evaluated in order. Independent inputs form explicit Cartesian
products; namespace templates correlate downstream inputs with earlier
bindings. Evaluation streams combinations rather than materializing a
Cartesian table. Administrators should still avoid unnecessarily large products.

Each active declared and allowlisted GVK has one informer shared across rules
and aliases. Allowlisting a kind alone does not start it. No kind is inferred
from selectors. Deleting the last referencing rule, changing its class, or
changing its inputs stops unused informers. An unsupported replacement kind
does not leave the old, no-longer-declared watcher running.

Informers list/watch at cluster scope, including Kubernetes streaming-list
support when available. Label filters and namespace joins are local. A
namespace template does not narrow API permissions or cache scope. Discovery,
list/watch and cache synchronization failures are errors, not empty input sets.

## Configuration and permissions

```yaml
reconcile:
  clusterSPIFFEIDs: false
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

`reconcile.clusterSPIFFEIDSets` defaults to false. An empty `allowedInputKinds`
allows only constants. The allowlist is exact, with no wildcard interpretation.
Core Pod and Secret inputs are forbidden even if explicitly allowlisted.
List kinds and subresources are invalid. Static-manifest mode cannot enable Sets.

The allowlist **does not grant RBAC**. Administrators separately grant read-only
permissions on exact API resources. The controller does not create roles,
grant wildcard access or widen permissions based on a resource. Custom kinds
require explicit permission for the API group's plural resource name.

The legacy `cacheNamespace`/`cacheNamespaces` configuration is rejected when
Sets are enabled rather than silently ignoring its scope restrictions.
Existing controllers retain their cache behavior when Sets are disabled.

The generated manager role adds only Set resource and status access. Existing
permissions are unchanged. The [minimal role](../examples/identity-set-rbac.yaml)
covers the joined sample and static bootstrap entries when webhooks and other
controllers are disabled. The [manager configuration](../examples/identity-set-controller.yaml)
does not modify the allowlist in response to resource contents.

## Coalescing and registration IDs

Identical SPIFFE ID, parent and selector sets coalesce within and across rules
in the shared store. Existing creation-time/UID preference selects the preferred
declaration; the others report masked entries. An entry remains while a valid
candidate declares it. Other entry fields follow the existing shared-store
preference rules. Valid static bootstrap declarations remain in the same pass.

New Set entries have deterministic IDs:
`<entryIDPrefix>.identityset.<scope UUID>.<tuple UUID>`. The first component is
omitted when no prefix is configured. The scope includes the trust domain,
cluster name and controller class; tuple keys include the identity, parent
and canonical selectors. Fields use length framing so arbitrary selector
types and values cannot collide through concatenation. Rule names, aliases,
input ordering and duplicate selectors do not change the ID.

Set-only reconciliation manages only its scoped IDs and does not adopt or
delete unrelated registrations. Disabling Sets retains those scoped entries.
When existing sources also run, their normal prefix and cleanup behavior
continues. Use exclusive `entryIDPrefix` values for independent managers.
An identical tuple outside the managed scope is not silently adopted; a
SPIRE conflict is reported.

This standalone implementation intentionally does **not** preserve the earlier
v0.6.2-derived POC's `serviceaccount.` registration IDs or its special fixed
namespace/service-account ID algorithm. Even equivalent Set tuples use the
new `identityset.` namespace when newly created. There is no POC migration
controller or implicit handoff. Do not treat this implementation as a
registration-preserving replacement for that deployed POC. Existing managed
upstream registrations still reuse their current IDs when reconciling equal
tuples through the shared store.

## Status and failures

`status.observedGeneration` and the `Ready` condition identify the observed
generation. `stats.combinations` counts complete bindings, including the single
constant candidate. `entriesToSet` counts unique preferred declarations;
`entriesMasked` counts masked declarations. Render and SPIRE write failures
have separate counters.

Invalid specs report `InvalidSpec`. Template execution, selector, namespace
and identity errors report `RenderFailed`. Invalid rules and failed candidates
declare no entries, so their old registrations are collected on a successful
pass rather than retaining weaker constraints.

Discovery, allowlist, list/watch and unsynchronized-cache failures report
`InputUnavailable` and abort writes. Existing registrations are retained.
A live watch error is not hidden behind the last cached list. Recovery retries
on events and `gcInterval`. SPIRE failures report `SPIREAPIError`. Status-write
failures and deletion errors after a rule is gone are logged.

`Ready=True` describes registration reconciliation, not successful SVID
issuance. A fully masked rule can be Ready; inspect the preferred declaration
and actual registrations as well.

## Authorization boundary

Input selection determines declaration inputs, not workload authorization.
Only rendered selectors and SPIRE parent/attestation rules constrain issuance.
Removing namespace/service-account selectors can authorize workloads outside
the selected input objects. Identity path names do not add those bindings.
Metadata and ConfigMap content used in templates are identity-authorizing
configuration; restrict their mutation.

The controller does not prove a workload attestor emits an arbitrary selector.
Verify issuance and rejection against the intended attestation path. Removing
registrations does not instantly revoke previously issued SVIDs.
