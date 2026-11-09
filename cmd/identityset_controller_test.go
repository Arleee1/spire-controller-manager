package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	"github.com/spiffe/spire-controller-manager/pkg/k8sinput"
	"github.com/spiffe/spire-controller-manager/pkg/spireapi"
	"github.com/spiffe/spire-controller-manager/pkg/spireentry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func TestIdentitySetActualInputRequests(t *testing.T) {
	config, direct := identitySetEnvironment(t)
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "set-inputs", Labels: map[string]string{"set-input": "true"}}}
	require.NoError(t, direct.Create(ctx, ns))
	for _, name := range []string{"one", "two"} {
		require.NoError(t, direct.Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name, Labels: map[string]string{"set-input": "true"}}}))
	}
	require.NoError(t, direct.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "input", Namespace: ns.Name}, Data: map[string]string{"identity": "configmap", "selector": "payload:with:colons"}}))
	widget := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "inputs.example.org/v1", "kind": "Widget",
		"metadata": map[string]any{"name": "input", "namespace": ns.Name}, "spec": map[string]any{"identity": "widget", "selector": "custom:resource"},
	}}
	require.NoError(t, direct.Create(ctx, widget))
	invalid := actualSet("invalid-schema")
	invalid.Spec.WorkloadSelectorTemplates = []string{}
	require.True(t, apierrors.IsInvalid(direct.Create(ctx, invalid)), "empty selector list must fail admission")
	invalid.Spec = actualSet("unused").Spec
	invalid.Spec.ForEach = []spirev1alpha1.SPIFFEIDSetInput{actualInput("invalid-alias", "Namespace", "", false)}
	require.True(t, apierrors.IsInvalid(direct.Create(ctx, invalid)), "invalid aliases must fail admission")
	t.Run("standalone CR events without periodic reconciler", func(t *testing.T) {
		mgr, err := ctrl.NewManager(config, ctrl.Options{
			Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"},
			Controller: controllerconfig.Controller{SkipNameValidation: ptr.To(true)},
		})
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(context.Background())
		trigger := &serviceAccountTrigger{}
		require.NoError(t, setupEntryControllers(ctx, mgr, Config{reconcile: spirev1alpha1.ReconcileConfig{ClusterSPIFFEIDSets: true}}, trigger))
		done := make(chan error, 1)
		go func() { done <- mgr.Start(ctx) }()
		t.Cleanup(func() {
			cancel()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(10 * time.Second):
				t.Error("standalone generic controller did not stop")
			}
		})
		rule := actualSet("events-only")
		require.NoError(t, direct.Create(ctx, rule))
		require.Eventually(t, func() bool { return trigger.calls.Load() > 0 }, 5*time.Second, 20*time.Millisecond)
		before := trigger.calls.Load()
		require.NoError(t, direct.Get(ctx, client.ObjectKeyFromObject(rule), rule))
		rule.Spec.WorkloadSelectorTemplates = []string{"custom:changed"}
		require.NoError(t, direct.Update(ctx, rule))
		require.Eventually(t, func() bool { return trigger.calls.Load() > before }, 5*time.Second, 20*time.Millisecond)
		before = trigger.calls.Load()
		require.NoError(t, direct.Delete(ctx, rule))
		require.Eventually(t, func() bool { return trigger.calls.Load() > before }, 5*time.Second, 20*time.Millisecond)
	})
	for _, tt := range []struct {
		name, identity string
		inputs         []spirev1alpha1.SPIFFEIDSetInput
		selectors      []string
		want           int
		resources      []string
	}{
		{"constant", "spiffe://{{ .TrustDomain }}/constant", nil, []string{"custom:constant"}, 1, nil},
		{"namespace", "spiffe://{{ .TrustDomain }}/ns/{{ .Inputs.ns.metadata.name }}", []spirev1alpha1.SPIFFEIDSetInput{actualInput("ns", "Namespace", "", true)}, []string{"k8s:ns:{{ .Inputs.ns.metadata.name }}"}, 1, []string{"namespaces"}},
		{"serviceaccount", "spiffe://{{ .TrustDomain }}/ns/{{ .Inputs.sa.metadata.namespace }}/sa/{{ .Inputs.sa.metadata.name }}", []spirev1alpha1.SPIFFEIDSetInput{actualInput("sa", "ServiceAccount", "", true)}, []string{"k8s:sa:{{ .Inputs.sa.metadata.name }}"}, 2, []string{"serviceaccounts"}},
		{"joined", "spiffe://{{ .TrustDomain }}/ns/{{ .Inputs.ns.metadata.name }}/sa/{{ .Inputs.sa.metadata.name }}", []spirev1alpha1.SPIFFEIDSetInput{actualInput("ns", "Namespace", "", true), actualInput("sa", "ServiceAccount", "{{ .Inputs.ns.metadata.name }}", true)}, []string{"k8s:ns:{{ .Inputs.ns.metadata.name }}", "k8s:sa:{{ .Inputs.sa.metadata.name }}"}, 2, []string{"namespaces", "serviceaccounts"}},
		{"configmap", "spiffe://{{ .TrustDomain }}/{{ .Inputs.config.data.identity }}", []spirev1alpha1.SPIFFEIDSetInput{actualInput("config", "ConfigMap", "set-inputs", false)}, []string{"custom:{{ .Inputs.config.data.selector }}"}, 1, []string{"configmaps"}},
		{"customresource", "spiffe://{{ .TrustDomain }}/{{ .Inputs.widget.spec.identity }}", []spirev1alpha1.SPIFFEIDSetInput{{SPIFFEIDSetKind: spirev1alpha1.SPIFFEIDSetKind{APIVersion: "inputs.example.org/v1", Kind: "Widget"}, Alias: "widget"}}, []string{"custom:{{ .Inputs.widget.spec.selector }}"}, 1, []string{"widgets"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rule := actualSet(tt.name)
			rule.Spec.ForEach, rule.Spec.SPIFFEIDTemplate, rule.Spec.WorkloadSelectorTemplates = tt.inputs, tt.identity, tt.selectors
			require.NoError(t, direct.Create(ctx, rule))
			t.Cleanup(func() { require.NoError(t, direct.Delete(ctx, rule)) })
			store, requests := startIdentitySetManager(t, config, rule.Spec.ClassName)
			waitForSet(t, direct, rule.Name, store, tt.want)
			for _, resource := range []string{"namespaces", "serviceaccounts", "configmaps", "widgets", "pods", "nodes", "endpoints", "endpointslices", "secrets", "clusterspiffeids", "clusterserviceaccountspiffeids"} {
				if slices.Contains(tt.resources, resource) {
					require.Positive(t, requests.count(resource, false), "%s must be listed", resource)
					require.Positive(t, requests.count(resource, true), "%s must be watched", resource)
				} else {
					require.Zero(t, requests.count(resource, false), "excluded kind %s was requested", resource)
					require.Zero(t, requests.count(resource, true), "excluded kind %s was watched", resource)
				}
			}
			t.Logf("%s: expected inputs=%v, registrations=%d, request counts=%v", tt.name, tt.resources, tt.want, requests.snapshot())
		})
	}

	t.Run("independent aliases share one GVK informer", func(t *testing.T) {
		for _, dimension := range []string{"team", "environment"} {
			for _, value := range []string{"one", "two"} {
				require.NoError(t, direct.Create(ctx, &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: dimension + "-" + value, Namespace: "set-inputs", Labels: map[string]string{"dimension": dimension}},
					Data:       map[string]string{"value": value},
				}))
			}
		}
		team := actualInput("team", "ConfigMap", "set-inputs", false)
		team.LabelSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"dimension": "team"}}
		environment := actualInput("environment", "ConfigMap", "set-inputs", false)
		environment.LabelSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"dimension": "environment"}}
		rule := actualSet("independent")
		rule.Spec.ForEach = []spirev1alpha1.SPIFFEIDSetInput{team, environment}
		rule.Spec.SPIFFEIDTemplate = "spiffe://{{ .TrustDomain }}/team/{{ .Inputs.team.data.value }}/environment/{{ .Inputs.environment.data.value }}"
		rule.Spec.WorkloadSelectorTemplates = []string{"custom:team:{{ .Inputs.team.data.value }}", "custom:environment:{{ .Inputs.environment.data.value }}"}
		require.NoError(t, direct.Create(ctx, rule))
		store, requests := startIdentitySetManager(t, config, rule.Spec.ClassName)
		waitForSet(t, direct, rule.Name, store, 4)
		var actual []string
		for _, entry := range store.snapshot() {
			actual = append(actual, entry.SPIFFEID.String())
		}
		require.ElementsMatch(t, []string{
			"spiffe://example.org/team/one/environment/one", "spiffe://example.org/team/one/environment/two",
			"spiffe://example.org/team/two/environment/one", "spiffe://example.org/team/two/environment/two",
		}, actual)
		require.Equal(t, 1, requests.count("configmaps", false))
		require.Equal(t, 1, requests.count("configmaps", true))
		require.Equal(t, 1, requests.active("configmaps"))
		original := store.snapshot()
		require.NoError(t, direct.Get(ctx, client.ObjectKeyFromObject(rule), rule))
		slices.Reverse(rule.Spec.ForEach)
		slices.Reverse(rule.Spec.WorkloadSelectorTemplates)
		rule.Spec.WorkloadSelectorTemplates = append(rule.Spec.WorkloadSelectorTemplates, rule.Spec.WorkloadSelectorTemplates[0])
		require.NoError(t, direct.Update(ctx, rule))
		waitForSet(t, direct, rule.Name, store, 4)
		require.ElementsMatch(t, original, store.snapshot(), "independent input and selector order must preserve all stored tuples and IDs")
		duplicate := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "team-duplicate", Namespace: "set-inputs", Labels: map[string]string{"dimension": "team"}},
			Data:       map[string]string{"value": "one"},
		}
		require.NoError(t, direct.Create(ctx, duplicate))
		require.Eventually(t, func() bool {
			var current spirev1alpha1.ClusterSPIFFEIDSet
			return direct.Get(ctx, client.ObjectKeyFromObject(rule), &current) == nil && current.Status.Stats.Combinations == 6
		}, 5*time.Second, 20*time.Millisecond)
		require.ElementsMatch(t, original, store.snapshot(), "duplicate candidates must coalesce into the four original registrations")
		require.NoError(t, direct.Delete(ctx, duplicate))
		require.NoError(t, direct.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "team-two", Namespace: "set-inputs"}}))
		waitForSet(t, direct, rule.Name, store, 2)
		for _, entry := range store.snapshot() {
			require.Contains(t, entry.SPIFFEID.Path(), "/team/one/")
		}
		require.NoError(t, direct.Delete(ctx, rule))
		require.Eventually(t, func() bool { return len(store.snapshot()) == 0 && requests.active("configmaps") == 0 }, 5*time.Second, 20*time.Millisecond)
		for _, resource := range []string{"namespaces", "serviceaccounts", "widgets", "pods", "nodes", "endpoints", "secrets"} {
			require.Zero(t, requests.count(resource, false))
			require.Zero(t, requests.count(resource, true))
		}
		t.Logf("independent same-GVK aliases: four tuples, one informer, stable order/dedup, deletion cleanup; counts=%v", requests.snapshot())
	})

	t.Run("input transition and last-rule retirement", func(t *testing.T) {
		rule := actualSet("transition")
		rule.Spec.ForEach = []spirev1alpha1.SPIFFEIDSetInput{actualInput("ns", "Namespace", "", true)}
		rule.Spec.SPIFFEIDTemplate = "spiffe://{{ .TrustDomain }}/ns/{{ .Inputs.ns.metadata.name }}"
		require.NoError(t, direct.Create(ctx, rule))
		store, requests := startIdentitySetManager(t, config, rule.Spec.ClassName)
		waitForSet(t, direct, rule.Name, store, 1)
		require.Eventually(t, func() bool { return requests.active("namespaces") > 0 }, 5*time.Second, 20*time.Millisecond)
		require.NoError(t, direct.Get(ctx, client.ObjectKeyFromObject(rule), rule))
		rule.Spec.ForEach = []spirev1alpha1.SPIFFEIDSetInput{actualInput("config", "ConfigMap", "set-inputs", false)}
		rule.Spec.ForEach[0].LabelSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "dimension", Operator: metav1.LabelSelectorOpDoesNotExist}}}
		rule.Spec.SPIFFEIDTemplate = "spiffe://{{ .TrustDomain }}/{{ .Inputs.config.data.identity }}"
		require.NoError(t, direct.Update(ctx, rule))
		waitForSet(t, direct, rule.Name, store, 1)
		require.Eventually(t, func() bool { return requests.active("namespaces") == 0 && requests.active("configmaps") > 0 }, 5*time.Second, 20*time.Millisecond)
		require.Equal(t, "spiffe://example.org/configmap", store.snapshot()[0].SPIFFEID.String())
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "input", Namespace: "set-inputs"}}
		require.NoError(t, direct.Delete(ctx, cm))
		waitForSet(t, direct, rule.Name, store, 0)
		require.NoError(t, direct.Get(ctx, client.ObjectKeyFromObject(rule), rule))
		rule.Spec.ForEach = nil
		rule.Spec.SPIFFEIDTemplate = "spiffe://{{ .TrustDomain }}/constant"
		require.NoError(t, direct.Update(ctx, rule))
		waitForSet(t, direct, rule.Name, store, 1)
		require.Eventually(t, func() bool { return requests.active("configmaps") == 0 }, 5*time.Second, 20*time.Millisecond)
		require.NoError(t, direct.Delete(ctx, rule))
		require.Eventually(t, func() bool { return len(store.snapshot()) == 0 }, 5*time.Second, 20*time.Millisecond)
		for _, resource := range []string{"serviceaccounts", "widgets", "pods", "nodes", "endpoints", "secrets"} {
			require.Zero(t, requests.count(resource, false))
			require.Zero(t, requests.count(resource, true))
		}
	})
}

func TestIdentitySetActualRBACFailureRetainsEntries(t *testing.T) {
	config, direct := identitySetEnvironment(t)
	ctx := context.Background()
	require.NoError(t, direct.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "set-inputs"}}))
	require.NoError(t, direct.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "input", Namespace: "set-inputs"}, Data: map[string]string{"identity": "after-rbac"}}))
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "set-reader"}, Rules: []rbacv1.PolicyRule{
		{APIGroups: []string{"spire.spiffe.io"}, Resources: []string{"clusterspiffeidsets"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"spire.spiffe.io"}, Resources: []string{"clusterspiffeidsets/status"}, Verbs: []string{"get", "patch", "update"}},
	}}
	require.NoError(t, direct.Create(ctx, role))
	require.NoError(t, direct.Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "set-reader"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: "identity-set-reader"}},
	}))
	restricted := rest.CopyConfig(config)
	restricted.Impersonate = rest.ImpersonationConfig{UserName: "identity-set-reader", Groups: []string{"system:authenticated"}}
	rule := actualSet("restricted")
	require.NoError(t, direct.Create(ctx, rule))
	store, _ := startIdentitySetManager(t, restricted, rule.Spec.ClassName)
	waitForSet(t, direct, rule.Name, store, 1)
	initial := store.snapshot()[0]
	require.NoError(t, direct.Get(ctx, client.ObjectKeyFromObject(rule), rule))
	rule.Spec.ForEach = []spirev1alpha1.SPIFFEIDSetInput{actualInput("config", "ConfigMap", "set-inputs", false)}
	rule.Spec.SPIFFEIDTemplate = "spiffe://{{ .TrustDomain }}/{{ .Inputs.config.data.identity }}"
	require.NoError(t, direct.Update(ctx, rule))
	require.Eventually(t, func() bool {
		var current spirev1alpha1.ClusterSPIFFEIDSet
		if direct.Get(ctx, client.ObjectKeyFromObject(rule), &current) != nil {
			return false
		}
		condition := meta.FindStatusCondition(current.Status.Conditions, "Ready")
		return condition != nil && condition.Reason == "InputUnavailable" && strings.Contains(condition.Message, "forbidden")
	}, 15*time.Second, 30*time.Millisecond)
	require.Equal(t, []spireapi.Entry{initial}, store.snapshot(), "RBAC failure must not look like an empty input cache")
	require.NoError(t, direct.Get(ctx, client.ObjectKeyFromObject(role), role))
	role.Rules = append(role.Rules, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get", "list", "watch"}})
	require.NoError(t, direct.Update(ctx, role), "test administrator explicitly grants only ConfigMap reads")
	waitForSet(t, direct, rule.Name, store, 1)
	require.Equal(t, "spiffe://example.org/after-rbac", store.snapshot()[0].SPIFFEID.String())
}

func identitySetEnvironment(t *testing.T) (*rest.Config, client.Client) {
	t.Helper()
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	require.NoError(t, apiextensionsv1.AddToScheme(scheme))
	testEnv := &envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join("..", "config", "crd", "bases")}, ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: filepath.Join("..", "bin", "k8s", "1.28.0-"+runtime.GOOS+"-"+runtime.GOARCH),
		CRDs: []*apiextensionsv1.CustomResourceDefinition{{
			ObjectMeta: metav1.ObjectMeta{Name: "widgets.inputs.example.org"},
			Spec: apiextensionsv1.CustomResourceDefinitionSpec{
				Group: "inputs.example.org", Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: "Widget", Plural: "widgets"},
				Scope: apiextensionsv1.NamespaceScoped,
				Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1", Served: true, Storage: true, Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"spec": {Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{"identity": {Type: "string"}, "selector": {Type: "string"}}},
					}},
				}}},
			},
		}},
	}
	config, err := testEnv.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, testEnv.Stop()) })
	direct, err := client.New(config, client.Options{Scheme: scheme})
	require.NoError(t, err)
	return config, direct
}

func actualInput(alias, kind, namespace string, labelled bool) spirev1alpha1.SPIFFEIDSetInput {
	input := spirev1alpha1.SPIFFEIDSetInput{SPIFFEIDSetKind: spirev1alpha1.SPIFFEIDSetKind{APIVersion: "v1", Kind: kind}, Alias: alias}
	if namespace != "" {
		input.NamespaceTemplate = &namespace
	}
	if labelled {
		input.LabelSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"set-input": "true"}}
	}
	return input
}

func actualSet(name string) *spirev1alpha1.ClusterSPIFFEIDSet {
	return &spirev1alpha1.ClusterSPIFFEIDSet{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spirev1alpha1.ClusterSPIFFEIDSetSpec{
		ClassName: name, SPIFFEIDTemplate: "spiffe://{{ .TrustDomain }}/constant", ParentIDTemplate: "spiffe://{{ .TrustDomain }}/parent",
		WorkloadSelectorTemplates: []string{"custom:constant"},
	}}
}

func startIdentitySetManager(t *testing.T, config *rest.Config, class string) (*identitySetStore, *inputRequests) {
	t.Helper()
	requests := &inputRequests{counts: make(map[string]int), watches: make(map[string]int)}
	managerConfig := rest.CopyConfig(config)
	managerConfig.WrapTransport = func(base http.RoundTripper) http.RoundTripper {
		return &inputRequestTransport{base: base, requests: requests}
	}
	mgr, err := ctrl.NewManager(managerConfig, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"},
		// The test runs multiple sequential managers with the same controller name.
		Controller: controllerconfig.Controller{SkipNameValidation: ptr.To(true)},
	})
	require.NoError(t, err)
	dynamicClient, err := dynamic.NewForConfig(managerConfig)
	require.NoError(t, err)
	inputs, err := k8sinput.New(dynamicClient, mgr.GetRESTMapper(), []spirev1alpha1.SPIFFEIDSetKind{
		{APIVersion: "v1", Kind: "Namespace"}, {APIVersion: "v1", Kind: "ServiceAccount"}, {APIVersion: "v1", Kind: "ConfigMap"}, {APIVersion: "inputs.example.org/v1", Kind: "Widget"},
	})
	require.NoError(t, err)
	store := &identitySetStore{entries: make(map[string]spireapi.Entry)}
	runner := spireentry.Reconciler(spireentry.ReconcilerConfig{
		TrustDomain: spiffeid.RequireTrustDomainFromString("example.org"), ClusterName: "test", ClassName: class,
		K8sClient: mgr.GetClient(), Inputs: inputs, EntryClient: store, EntryIDPrefix: "set-test.", GCInterval: 100 * time.Millisecond,
		Reconcile: spirev1alpha1.ReconcileConfig{ClusterSPIFFEIDSets: true},
	})
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, setupEntryControllers(ctx, mgr, Config{reconcile: spirev1alpha1.ReconcileConfig{ClusterSPIFFEIDSets: true}}, runner))
	require.NoError(t, mgr.Add(manager.RunnableFunc(runner.Run)))
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				require.NoError(t, err)
			}
		case <-time.After(15 * time.Second):
			t.Error("generic input manager did not stop")
		}
	})
	return store, requests
}

func waitForSet(t *testing.T, c client.Client, name string, store *identitySetStore, count int) {
	t.Helper()
	var last spirev1alpha1.ClusterSPIFFEIDSet
	if !assert.Eventually(t, func() bool {
		var current spirev1alpha1.ClusterSPIFFEIDSet
		if c.Get(context.Background(), types.NamespacedName{Name: name}, &current) != nil {
			return false
		}
		last = current
		condition := meta.FindStatusCondition(current.Status.Conditions, "Ready")
		return condition != nil && condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == current.Generation && len(store.snapshot()) == count
	}, 20*time.Second, 30*time.Millisecond) {
		t.Fatalf("last identity set status: %+v; generation: %d; entries: %+v", last.Status, last.Generation, store.snapshot())
	}
}

type identitySetStore struct {
	mu      sync.Mutex
	entries map[string]spireapi.Entry
}

func (s *identitySetStore) snapshot() []spireapi.Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]spireapi.Entry, 0, len(s.entries))
	for _, entry := range s.entries {
		out = append(out, entry)
	}
	return out
}
func (s *identitySetStore) ListEntries(context.Context) ([]spireapi.Entry, error) {
	return s.snapshot(), nil
}
func (*identitySetStore) GetUnsupportedFields(context.Context, string) (map[spireapi.Field]struct{}, error) {
	return nil, nil
}
func (s *identitySetStore) CreateEntries(_ context.Context, entries []spireapi.Entry) ([]spireapi.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, entry := range entries {
		s.entries[entry.ID] = entry
	}
	return make([]spireapi.Status, len(entries)), nil
}
func (s *identitySetStore) UpdateEntries(ctx context.Context, entries []spireapi.Entry) ([]spireapi.Status, error) {
	return s.CreateEntries(ctx, entries)
}
func (s *identitySetStore) DeleteEntries(_ context.Context, ids []string) ([]spireapi.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		delete(s.entries, id)
	}
	return make([]spireapi.Status, len(ids)), nil
}

type inputRequests struct {
	mu      sync.Mutex
	counts  map[string]int
	watches map[string]int
}

func (r *inputRequests) count(resource string, watch bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[fmt.Sprintf("%s/watch=%v", resource, watch)]
}
func (r *inputRequests) active(resource string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.watches[resource]
}
func (r *inputRequests) snapshot() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int)
	for key, value := range r.counts {
		out[key] = value
	}
	return out
}

type inputRequestTransport struct {
	base     http.RoundTripper
	requests *inputRequests
}

func (t *inputRequestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
	index := 2
	if len(parts) != 0 && parts[0] == "apis" {
		index = 3
	}
	resource := ""
	if len(parts) > index {
		if parts[index] == "namespaces" && len(parts) > index+2 {
			index += 2
		}
		resource = parts[index]
	}
	watching := req.URL.Query().Get("watch") == "true"
	t.requests.mu.Lock()
	t.requests.counts[fmt.Sprintf("%s/watch=%v", resource, watching)]++
	t.requests.mu.Unlock()
	resp, err := t.base.RoundTrip(req)
	if err == nil && watching && resp.StatusCode == http.StatusOK {
		t.requests.mu.Lock()
		t.requests.watches[resource]++
		t.requests.mu.Unlock()
		resp.Body = &watchedInputBody{ReadCloser: resp.Body, close: func() {
			t.requests.mu.Lock()
			t.requests.watches[resource]--
			t.requests.mu.Unlock()
		}}
	}
	return resp, err
}

type watchedInputBody struct {
	io.ReadCloser
	once  sync.Once
	close func()
}

func (b *watchedInputBody) Close() error {
	b.once.Do(b.close)
	return b.ReadCloser.Close()
}
