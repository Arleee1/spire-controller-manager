package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	"github.com/stretchr/testify/require"
)

func TestIdentitySetConfigOptIn(t *testing.T) {
	for _, tt := range []struct {
		name, extra, err string
		want             spirev1alpha1.ReconcileConfig
	}{
		{name: "default remains unchanged", want: spirev1alpha1.ReconcileConfig{ClusterSPIFFEIDs: true, ClusterStaticEntries: true, ClusterFederatedTrustDomains: true}},
		{name: "static default remains unchanged", extra: "staticManifestPath: unused\n",
			want: spirev1alpha1.ReconcileConfig{ClusterStaticEntries: true, ClusterFederatedTrustDomains: true}},
		{name: "empty disables all", extra: "reconcile: {}\n"},
		{name: "constant opt in", extra: "reconcile:\n  clusterSPIFFEIDSets: true\n",
			want: spirev1alpha1.ReconcileConfig{ClusterSPIFFEIDSets: true}},
		{name: "generic opt in", extra: "reconcile:\n  clusterSPIFFEIDSets: true\n  clusterStaticEntries: true\nclusterSPIFFEIDSet:\n  allowedInputKinds:\n    - apiVersion: v1\n      kind: Namespace\n",
			want: spirev1alpha1.ReconcileConfig{ClusterSPIFFEIDSets: true, ClusterStaticEntries: true}},
		{name: "static mode rejected", extra: "staticManifestPath: unused\nreconcile:\n  clusterSPIFFEIDSets: true\n", err: "ClusterSPIFFEIDSet reconciliation is not possible with static manifests"},
		{name: "cache namespace rejected", extra: "cacheNamespace: restricted\nreconcile:\n  clusterSPIFFEIDSets: true\n", err: "ClusterSPIFFEIDSet does not support cacheNamespace/cacheNamespaces; use explicit input selectors and namespaceTemplate"},
		{name: "cache namespaces rejected", extra: "cacheNamespaces:\n  restricted: {}\nreconcile:\n  clusterSPIFFEIDSets: true\n", err: "ClusterSPIFFEIDSet does not support cacheNamespace/cacheNamespaces; use explicit input selectors and namespaceTemplate"},
		{name: "legacy scoped cache unchanged", extra: "cacheNamespace: restricted\n",
			want: spirev1alpha1.ReconcileConfig{ClusterSPIFFEIDs: true, ClusterStaticEntries: true, ClusterFederatedTrustDomains: true}},
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
			} else {
				require.Empty(t, config.ctrlConfig.ClusterSPIFFEIDSet.AllowedInputKinds)
			}
		})
	}
}
