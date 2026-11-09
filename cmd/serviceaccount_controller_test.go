package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func TestServiceAccountConfigOptIn(t *testing.T) {
	for _, tt := range []struct {
		name, extra, err string
		want             spirev1alpha1.ReconcileConfig
	}{
		{name: "default remains legacy", want: spirev1alpha1.ReconcileConfig{ClusterSPIFFEIDs: true, ClusterStaticEntries: true, ClusterFederatedTrustDomains: true}},
		{name: "explicit opt in", extra: "reconcile:\n  clusterSPIFFEIDs: false\n  clusterServiceAccountSPIFFEIDs: true\n  clusterStaticEntries: true\n",
			want: spirev1alpha1.ReconcileConfig{ClusterServiceAccountSPIFFEIDs: true, ClusterStaticEntries: true}},
		{name: "empty disables all", extra: "reconcile: {}\n"},
		{name: "generic opt in", extra: "reconcile:\n  clusterSPIFFEIDSets: true\n  clusterStaticEntries: true\nclusterSPIFFEIDSet:\n  allowedInputKinds:\n    - apiVersion: v1\n      kind: Namespace\n",
			want: spirev1alpha1.ReconcileConfig{ClusterSPIFFEIDSets: true, ClusterStaticEntries: true}},
		{name: "generic static mode rejected", extra: "staticManifestPath: unused\nreconcile:\n  clusterSPIFFEIDSets: true\n", err: "ClusterSPIFFEIDSet reconciliation is not possible with static manifests"},
		{name: "generic cache scope rejected", extra: "cacheNamespace: restricted\nreconcile:\n  clusterSPIFFEIDSets: true\n", err: "ClusterSPIFFEIDSet does not support cacheNamespace/cacheNamespaces; use explicit input selectors and namespaceTemplate"},
		{name: "static mode rejected", extra: "staticManifestPath: unused\nreconcile:\n  clusterServiceAccountSPIFFEIDs: true\n", err: "ClusterServiceAccountSPIFFEID reconciliation is not possible with static manifests"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			content := "apiVersion: spire.spiffe.io/v1alpha1\nkind: ControllerManagerConfig\ntrustDomain: example.org\nclusterName: test\nclusterDomain: cluster.local\n" + tt.extra
			require.NoError(t, os.WriteFile(configPath, []byte(content), 0600))
			oldFlags, oldArgs := flag.CommandLine, os.Args
			t.Cleanup(func() { flag.CommandLine, os.Args = oldFlags, oldArgs })
			flag.CommandLine = flag.NewFlagSet("config-test", flag.ContinueOnError)
			os.Args = []string{"config-test", "--config", configPath}
			config, err := parseConfig()
			if tt.err != "" {
				require.EqualError(t, err, tt.err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, config.reconcile)
			if tt.name == "generic opt in" {
				require.Equal(t, []spirev1alpha1.SPIFFEIDSetKind{{APIVersion: "v1", Kind: "Namespace"}}, config.ctrlConfig.ClusterSPIFFEIDSet.AllowedInputKinds)
			}
		})
	}
}

func TestServiceAccountControllerWatchesWithoutPodInformer(t *testing.T) {
	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: filepath.Join("..", "bin", "k8s", "1.28.0-"+runtime.GOOS+"-"+runtime.GOARCH),
	}
	config, err := testEnv.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, testEnv.Stop()) })
	direct, err := client.New(config, client.Options{Scheme: scheme})
	require.NoError(t, err)
	requests := &serviceAccountRequests{paths: map[string]int{}}
	managerConfig := rest.CopyConfig(config)
	managerConfig.WrapTransport = func(base http.RoundTripper) http.RoundTripper {
		return &serviceAccountTransport{base: base, requests: requests}
	}
	mgr, err := ctrl.NewManager(managerConfig, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"},
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	trigger := &serviceAccountTrigger{}
	require.NoError(t, setupEntryControllers(ctx, mgr, Config{
		reconcile: spirev1alpha1.ReconcileConfig{ClusterServiceAccountSPIFFEIDs: true, ClusterStaticEntries: true},
	}, trigger))
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Error("manager did not stop")
		}
	})
	require.Eventually(t, func() bool {
		for _, path := range []string{
			"/api/v1/namespaces", "/api/v1/serviceaccounts",
			"/apis/spire.spiffe.io/v1alpha1/clusterserviceaccountspiffeids",
			"/apis/spire.spiffe.io/v1alpha1/clusterstaticentries",
		} {
			if requests.count(path) < 2 {
				return false
			}
		}
		return true
	}, 15*time.Second, 20*time.Millisecond)
	require.True(t, mgr.GetCache().WaitForCacheSync(ctx))
	waitForQuiet := func() {
		t.Helper()
		require.Eventually(t, func() bool {
			before := trigger.calls.Load()
			time.Sleep(100 * time.Millisecond)
			return trigger.calls.Load() == before
		}, 5*time.Second, 20*time.Millisecond)
	}
	event := func(change func()) {
		t.Helper()
		waitForQuiet()
		before := trigger.calls.Load()
		change()
		require.Eventually(t, func() bool { return trigger.calls.Load() > before }, 5*time.Second, 20*time.Millisecond)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "sa-watch-test"}}
	event(func() { require.NoError(t, direct.Create(ctx, ns)) })
	event(func() {
		require.NoError(t, direct.Get(ctx, client.ObjectKeyFromObject(ns), ns))
		ns.Labels = map[string]string{"io.cilium/mtls-enabled": "true"}
		require.NoError(t, direct.Update(ctx, ns))
	})
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: ns.Name}}
	event(func() { require.NoError(t, direct.Create(ctx, sa)) })
	event(func() {
		require.NoError(t, direct.Get(ctx, client.ObjectKeyFromObject(sa), sa))
		sa.Labels = map[string]string{"role": "app"}
		require.NoError(t, direct.Update(ctx, sa))
	})
	event(func() {
		require.NoError(t, direct.Get(ctx, client.ObjectKeyFromObject(sa), sa))
		sa.Annotations = map[string]string{"example.org/selector": "custom:value:with:colons"}
		require.NoError(t, direct.Update(ctx, sa))
	})
	rule := &spirev1alpha1.ClusterServiceAccountSPIFFEID{
		ObjectMeta: metav1.ObjectMeta{Name: "watch-test"},
		Spec: spirev1alpha1.ClusterServiceAccountSPIFFEIDSpec{
			NamespaceSelector: metav1.LabelSelector{MatchLabels: map[string]string{"io.cilium/mtls-enabled": "true"}},
			SPIFFEIDTemplate:  "spiffe://{{ .TrustDomain }}/ns/{{ .NamespaceMeta.Name }}/sa/{{ .ServiceAccountMeta.Name }}",
			ParentIDTemplate:  "spiffe://{{ .TrustDomain }}/ztunnel",
		},
	}
	event(func() { require.NoError(t, direct.Create(ctx, rule)) })
	event(func() {
		require.NoError(t, direct.Get(ctx, client.ObjectKeyFromObject(rule), rule))
		require.Nil(t, rule.Spec.WorkloadSelectorTemplates)
		rule.Spec.ClassName = "new-class"
		rule.Spec.WorkloadSelectorTemplates = &[]string{
			"k8s:ns:{{ .NamespaceMeta.Name }}",
			"k8s:sa:{{ .ServiceAccountMeta.Name }}",
			`{{ index .ServiceAccountMeta.Annotations "example.org/selector" }}`,
		}
		require.NoError(t, direct.Update(ctx, rule))
	})
	require.NoError(t, direct.Get(ctx, client.ObjectKeyFromObject(rule), rule))
	require.Equal(t, []string{
		"k8s:ns:{{ .NamespaceMeta.Name }}",
		"k8s:sa:{{ .ServiceAccountMeta.Name }}",
		`{{ index .ServiceAccountMeta.Annotations "example.org/selector" }}`,
	}, *rule.Spec.WorkloadSelectorTemplates)
	event(func() { require.NoError(t, direct.Delete(ctx, sa)) })
	event(func() { require.NoError(t, direct.Delete(ctx, ns)) })
	event(func() { require.NoError(t, direct.Delete(ctx, rule)) })
	waitForQuiet()

	invalid := rule.DeepCopy()
	invalid.ObjectMeta = metav1.ObjectMeta{Name: "invalid-template"}
	invalid.Spec.SPIFFEIDTemplate = ""
	require.True(t, apierrors.IsInvalid(direct.Create(ctx, invalid)), "schema must reject empty identity templates")
	invalid.Spec.SPIFFEIDTemplate = rule.Spec.SPIFFEIDTemplate
	invalid.Spec.ParentIDTemplate = ""
	require.True(t, apierrors.IsInvalid(direct.Create(ctx, invalid)), "schema must reject empty parent templates")
	invalid.Spec.ParentIDTemplate = rule.Spec.ParentIDTemplate
	invalid.Spec.WorkloadSelectorTemplates = &[]string{}
	require.True(t, apierrors.IsInvalid(direct.Create(ctx, invalid)), "schema must reject an explicitly empty selector list")
	invalid.Spec.WorkloadSelectorTemplates = &[]string{""}
	require.True(t, apierrors.IsInvalid(direct.Create(ctx, invalid)), "schema must reject an empty selector template")

	nullable := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "spire.spiffe.io/v1alpha1",
		"kind":       "ClusterServiceAccountSPIFFEID",
		"metadata":   map[string]any{"name": "null-selectors"},
		"spec": map[string]any{
			"namespaceSelector":         map[string]any{},
			"spiffeIDTemplate":          rule.Spec.SPIFFEIDTemplate,
			"parentIDTemplate":          rule.Spec.ParentIDTemplate,
			"workloadSelectorTemplates": nil,
		},
	}}
	require.NoError(t, direct.Create(ctx, nullable))
	var nullRule spirev1alpha1.ClusterServiceAccountSPIFFEID
	require.NoError(t, direct.Get(ctx, client.ObjectKeyFromObject(nullable), &nullRule))
	require.Nil(t, nullRule.Spec.WorkloadSelectorTemplates, "null is pruned to the omitted/default behavior")
	require.NoError(t, direct.Delete(ctx, nullable))

	for path := range requests.snapshot() {
		for _, forbidden := range []string{"/pods", "/nodes", "/endpoints", "/clusterspiffeids"} {
			require.False(t, strings.HasSuffix(path, forbidden) || strings.Contains(path, forbidden+"/"), "unexpected manager API request: %s", path)
		}
	}
	var missing corev1.ServiceAccount
	require.True(t, apierrors.IsNotFound(direct.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: sa.Name}, &missing)))
}

type serviceAccountTrigger struct{ calls atomic.Int64 }

func (t *serviceAccountTrigger) Trigger() { t.calls.Add(1) }

type serviceAccountRequests struct {
	mu    sync.Mutex
	paths map[string]int
}

type serviceAccountTransport struct {
	base     http.RoundTripper
	requests *serviceAccountRequests
}

func (r *serviceAccountTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.requests.mu.Lock()
	r.requests.paths[req.URL.Path]++
	r.requests.mu.Unlock()
	return r.base.RoundTrip(req)
}

func (r *serviceAccountRequests) count(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.paths[path]
}

func (r *serviceAccountRequests) snapshot() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int, len(r.paths))
	for key, value := range r.paths {
		out[key] = value
	}
	return out
}
