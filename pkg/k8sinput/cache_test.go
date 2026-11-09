package k8sinput

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestDeclaredInputCacheLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, client := newTestCache(t,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team"}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "one", Namespace: "team", Labels: map[string]string{"selected": "true"}}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "two", Namespace: "other"}},
	)
	defer c.Close()
	require.NoError(t, c.Sync(ctx, nil))
	require.Empty(t, client.Actions())
	ns, err := c.Resolve(corev1.SchemeGroupVersion.WithKind("Namespace"))
	require.NoError(t, err)
	require.NoError(t, c.Sync(ctx, []Kind{ns}))
	require.Eventually(t, func() bool { return c.Check(ns.GVK) == nil }, 5*time.Second, 10*time.Millisecond)
	objects, err := c.List(ns.GVK, "", labels.Everything())
	require.NoError(t, err)
	require.Len(t, objects, 1)
	_, err = c.List(ns.GVK, "team", labels.Everything())
	require.ErrorContains(t, err, "cluster-scoped")
	for _, action := range client.Actions() {
		require.Equal(t, "namespaces", action.GetResource().Resource)
	}
	old := c.watchers[ns.GVK]
	sa, err := c.Resolve(corev1.SchemeGroupVersion.WithKind("ServiceAccount"))
	require.NoError(t, err)
	require.NoError(t, c.Sync(ctx, []Kind{sa, sa}))
	select {
	case <-old.done:
	default:
		t.Fatal("old input informer was not stopped before Sync returned")
	}
	require.ErrorContains(t, c.Check(ns.GVK), "no declared informer")
	require.Eventually(t, func() bool { return c.Check(sa.GVK) == nil }, 5*time.Second, 10*time.Millisecond)
	require.Len(t, c.watchers, 1, "aliases share one informer")
	objects, err = c.List(sa.GVK, "team", labels.SelectorFromSet(map[string]string{"selected": "true"}))
	require.NoError(t, err)
	require.Len(t, objects, 1)
	require.Equal(t, "one", objects[0].GetName())
	objects[0].SetName("mutated")
	objects, err = c.List(sa.GVK, "team", labels.Everything())
	require.NoError(t, err)
	require.Equal(t, "one", objects[0].GetName(), "returned objects cannot mutate the informer")
	objects, err = c.List(sa.GVK, "team", labels.SelectorFromSet(map[string]string{"selected": "false"}))
	require.NoError(t, err)
	require.Empty(t, objects)
	require.NoError(t, c.Sync(ctx, nil))
	require.Empty(t, c.watchers)
}

func TestInputCacheDoesNotReadUnallowedKinds(t *testing.T) {
	c, client := newTestCache(t)
	defer c.Close()
	for _, gvk := range []schema.GroupVersionKind{
		corev1.SchemeGroupVersion.WithKind("Pod"), corev1.SchemeGroupVersion.WithKind("Secret"),
		corev1.SchemeGroupVersion.WithKind("Node"), {Group: "unknown.example.org", Version: "v1", Kind: "Unknown"},
		{Version: "v2", Kind: "ConfigMap"},
	} {
		_, err := c.Resolve(gvk)
		require.ErrorContains(t, err, "not in")
	}
	require.Empty(t, client.Actions())
	constantOnly, err := New(client, c.mapper, nil)
	require.NoError(t, err)
	require.NoError(t, constantOnly.Sync(context.Background(), nil))
	_, err = constantOnly.Resolve(corev1.SchemeGroupVersion.WithKind("Namespace"))
	require.ErrorContains(t, err, "not in")
	require.Empty(t, client.Actions(), "empty allowlist must not access any workload inputs")
	for _, kind := range []string{"Pod", "Secret"} {
		_, err := New(client, c.mapper, []spirev1alpha1.SPIFFEIDSetKind{{APIVersion: "v1", Kind: kind}})
		require.ErrorContains(t, err, "forbidden")
	}
}

func TestInputCacheDiscoveryFailureRetiresOldKind(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, client := newTestCache(t)
	defer c.Close()
	ns, err := c.Resolve(corev1.SchemeGroupVersion.WithKind("Namespace"))
	require.NoError(t, err)
	require.NoError(t, c.Sync(ctx, []Kind{ns}))
	require.Eventually(t, func() bool { return c.Check(ns.GVK) == nil }, 5*time.Second, 10*time.Millisecond)
	missing := schema.GroupVersionKind{Group: "missing.example.org", Version: "v1", Kind: "Missing"}
	c.allowed[missing] = struct{}{}
	c.Retain([]schema.GroupVersionKind{missing})
	_, err = c.Resolve(missing)
	require.ErrorContains(t, err, "cannot discover")
	require.Empty(t, c.watchers)
	for _, action := range client.Actions() {
		require.Equal(t, "namespaces", action.GetResource().Resource)
	}
}

func TestInputCacheListAndWatchFailuresAreNotEmptySnapshots(t *testing.T) {
	for _, failure := range []string{"list", "watch"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c, client := newTestCache(t, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "input", Namespace: "team"}})
			defer c.Close()
			var broken atomic.Bool
			broken.Store(true)
			if failure == "list" {
				client.PrependReactor("list", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
					if broken.Load() {
						return true, nil, errors.New("Forbidden: configmaps list denied")
					}
					return false, nil, nil
				})
			} else {
				client.PrependWatchReactor("configmaps", func(clienttesting.Action) (bool, watch.Interface, error) {
					if broken.Load() {
						return true, nil, errors.New("Forbidden: configmaps watch denied")
					}
					return false, nil, nil
				})
			}
			kind, err := c.Resolve(corev1.SchemeGroupVersion.WithKind("ConfigMap"))
			require.NoError(t, err)
			require.NoError(t, c.Sync(ctx, []Kind{kind}))
			require.Eventually(t, func() bool {
				err := c.Check(kind.GVK)
				return err != nil && strings.Contains(err.Error(), "Forbidden")
			}, 5*time.Second, 10*time.Millisecond)
			objects, err := c.List(kind.GVK, "", labels.Everything())
			require.Error(t, err)
			require.Nil(t, objects)
			broken.Store(false)
			require.Eventually(t, func() bool { return c.Check(kind.GVK) == nil }, 15*time.Second, 20*time.Millisecond)
			objects, err = c.List(kind.GVK, "", labels.Everything())
			require.NoError(t, err)
			require.Len(t, objects, 1)
		})
	}
}

func TestInputCacheLiveWatchFailureBlocksCachedSnapshot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, client := newTestCache(t, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "input", Namespace: "team"}})
	defer c.Close()
	streams := make(chan *watch.RaceFreeFakeWatcher, 8)
	var broken atomic.Bool
	client.PrependWatchReactor("configmaps", func(clienttesting.Action) (bool, watch.Interface, error) {
		if broken.Load() {
			return true, nil, errors.New("Forbidden: live watch denied")
		}
		stream := watch.NewRaceFreeFake()
		streams <- stream
		return true, stream, nil
	})
	kind, err := c.Resolve(corev1.SchemeGroupVersion.WithKind("ConfigMap"))
	require.NoError(t, err)
	require.NoError(t, c.Sync(ctx, []Kind{kind}))
	require.Eventually(t, func() bool { return c.Check(kind.GVK) == nil }, 5*time.Second, 10*time.Millisecond)
	stream := <-streams
	broken.Store(true)
	stream.Error(&metav1.Status{Status: metav1.StatusFailure, Code: 403, Reason: metav1.StatusReasonForbidden, Message: "live watch denied"})
	require.Eventually(t, func() bool { return c.Check(kind.GVK) != nil }, 5*time.Second, 10*time.Millisecond)
	require.Len(t, c.watchers[kind.GVK].informer.GetIndexer().List(), 1)
	objects, err := c.List(kind.GVK, "", labels.Everything())
	require.Error(t, err)
	require.Nil(t, objects, "an old cache must not hide a live watch error")
	broken.Store(false)
	require.Eventually(t, func() bool { return c.Check(kind.GVK) == nil }, 15*time.Second, 20*time.Millisecond)
	objects, err = c.List(kind.GVK, "", labels.Everything())
	require.NoError(t, err)
	require.Len(t, objects, 1)
}

func newTestCache(t *testing.T, objects ...runtime.Object) (*Cache, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	client := dynamicfake.NewSimpleDynamicClient(scheme, objects...)
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{corev1.SchemeGroupVersion})
	var allowed []spirev1alpha1.SPIFFEIDSetKind
	for _, kind := range []string{"Namespace", "ServiceAccount", "ConfigMap"} {
		scope := meta.RESTScopeNamespace
		if kind == "Namespace" {
			scope = meta.RESTScopeRoot
		}
		mapper.Add(corev1.SchemeGroupVersion.WithKind(kind), scope)
		allowed = append(allowed, spirev1alpha1.SPIFFEIDSetKind{APIVersion: "v1", Kind: kind})
	}
	c, err := New(client, mapper, allowed)
	require.NoError(t, err)
	return c, client
}
