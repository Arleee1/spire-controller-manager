package k8sinput

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Kind is a discovered, allowlisted resource with an explicit namespace scope.
type Kind struct {
	GVK        schema.GroupVersionKind
	Resource   schema.GroupVersionResource
	Namespaced bool
}

// Source only exposes explicitly configured input informers, not a lazy client cache.
type Source interface {
	Resolve(schema.GroupVersionKind) (Kind, error)
	Retain([]schema.GroupVersionKind)
	Sync(context.Context, []Kind) error
	Check(schema.GroupVersionKind) error
	List(schema.GroupVersionKind, string, labels.Selector) ([]unstructured.Unstructured, error)
	SetTrigger(func())
	Close()
}

// Cache owns only the informers requested by active identity-set rules.
type Cache struct {
	client  dynamic.Interface
	mapper  meta.RESTMapper
	allowed map[schema.GroupVersionKind]struct{}

	mu       sync.RWMutex
	watchers map[schema.GroupVersionKind]*inputWatcher
	trigger  func()
}

// New validates an exact kind allowlist. It does not discover, list or watch inputs.
func New(client dynamic.Interface, mapper meta.RESTMapper, allowed []spirev1alpha1.SPIFFEIDSetKind) (*Cache, error) {
	c := &Cache{client: client, mapper: mapper, allowed: make(map[schema.GroupVersionKind]struct{}), watchers: make(map[schema.GroupVersionKind]*inputWatcher)}
	for _, kind := range allowed {
		gvk, err := spirev1alpha1.ParseSPIFFEIDSetKind(kind)
		if err != nil {
			return nil, fmt.Errorf("allowedInputKinds: %w", err)
		}
		c.allowed[gvk] = struct{}{}
	}
	return c, nil
}

func (c *Cache) Resolve(gvk schema.GroupVersionKind) (Kind, error) {
	if _, ok := c.allowed[gvk]; !ok {
		return Kind{}, fmt.Errorf("input kind %s is not in clusterSPIFFEIDSet.allowedInputKinds", gvk)
	}
	mapping, err := c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return Kind{}, fmt.Errorf("cannot discover input kind %s: %w", gvk, err)
	}
	if strings.Contains(mapping.Resource.Resource, "/") ||
		(mapping.Resource.Group == "" && (mapping.Resource.Resource == "pods" || mapping.Resource.Resource == "secrets")) {
		return Kind{}, fmt.Errorf("input resource %s is forbidden", mapping.Resource)
	}
	return Kind{GVK: gvk, Resource: mapping.Resource, Namespaced: mapping.Scope.Name() == meta.RESTScopeNameNamespace}, nil
}

// Sync starts declared kinds and stops unused kinds before returning.
func (c *Cache) Sync(ctx context.Context, kinds []Kind) error {
	wanted := make(map[schema.GroupVersionKind]Kind, len(kinds))
	var gvks []schema.GroupVersionKind
	for _, kind := range kinds {
		gvks = append(gvks, kind.GVK)
	}
	c.Retain(gvks)
	for _, kind := range kinds {
		resolved, err := c.Resolve(kind.GVK)
		if err != nil {
			return err
		}
		if resolved != kind {
			return fmt.Errorf("input mapping changed for %s", kind.GVK)
		}
		wanted[kind.GVK] = kind
	}

	var stopped []*inputWatcher
	c.mu.Lock()
	for gvk, watcher := range c.watchers {
		if kind, ok := wanted[gvk]; !ok || kind != watcher.kind {
			watcher.cancel()
			stopped = append(stopped, watcher)
			delete(c.watchers, gvk)
		}
	}
	c.mu.Unlock()
	for _, watcher := range stopped {
		<-watcher.done
	}
	for gvk, kind := range wanted {
		c.mu.RLock()
		existing := c.watchers[gvk]
		c.mu.RUnlock()
		if existing != nil {
			continue
		}
		watcher, err := c.newWatcher(ctx, kind)
		if err != nil {
			return err
		}
		c.mu.Lock()
		c.watchers[gvk] = watcher
		c.mu.Unlock()
		go func() {
			defer close(watcher.done)
			watcher.informer.Run(watcher.ctx.Done())
		}()
	}
	return nil
}

// Retain stops kinds no longer declared, even if discovery of new kinds fails.
func (c *Cache) Retain(kinds []schema.GroupVersionKind) {
	wanted := make(map[schema.GroupVersionKind]struct{}, len(kinds))
	for _, kind := range kinds {
		wanted[kind] = struct{}{}
	}
	var stopped []*inputWatcher
	c.mu.Lock()
	for gvk, watcher := range c.watchers {
		if _, ok := wanted[gvk]; !ok {
			watcher.cancel()
			stopped = append(stopped, watcher)
			delete(c.watchers, gvk)
		}
	}
	c.mu.Unlock()
	for _, watcher := range stopped {
		<-watcher.done
	}
}

func (c *Cache) checkedWatcher(gvk schema.GroupVersionKind) (*inputWatcher, error) {
	c.mu.RLock()
	watcher := c.watchers[gvk]
	c.mu.RUnlock()
	if watcher == nil {
		return nil, fmt.Errorf("input kind %s has no declared informer", gvk)
	}
	watcher.mu.RLock()
	err, watching := watcher.err, watcher.watching
	watcher.mu.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("input kind %s is unavailable: %w", gvk, err)
	}
	if !watcher.informer.HasSynced() || !watching || watcher.ctx.Err() != nil {
		return nil, fmt.Errorf("input kind %s has not synchronized a healthy list/watch", gvk)
	}
	return watcher, nil
}

func (c *Cache) Check(gvk schema.GroupVersionKind) error {
	_, err := c.checkedWatcher(gvk)
	return err
}

func (c *Cache) List(gvk schema.GroupVersionKind, namespace string, selector labels.Selector) ([]unstructured.Unstructured, error) {
	watcher, err := c.checkedWatcher(gvk)
	if err != nil {
		return nil, err
	}
	var objects []any
	if namespace != "" {
		if !watcher.kind.Namespaced {
			return nil, fmt.Errorf("cluster-scoped input kind %s cannot use a namespace", gvk)
		}
		objects, err = watcher.informer.GetIndexer().ByIndex(cache.NamespaceIndex, namespace)
		if err != nil {
			return nil, fmt.Errorf("input kind %s namespace index: %w", gvk, err)
		}
	} else {
		objects = watcher.informer.GetIndexer().List()
	}
	out := make([]unstructured.Unstructured, 0, len(objects))
	for _, object := range objects {
		value, ok := object.(*unstructured.Unstructured)
		if !ok {
			return nil, fmt.Errorf("unexpected object %T in input kind %s", object, gvk)
		}
		if value.GetDeletionTimestamp() == nil && selector.Matches(labels.Set(value.GetLabels())) {
			out = append(out, *value.DeepCopy())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GetNamespace() != out[j].GetNamespace() {
			return out[i].GetNamespace() < out[j].GetNamespace()
		}
		return out[i].GetName() < out[j].GetName()
	})
	return out, nil
}

func (c *Cache) SetTrigger(trigger func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.trigger = trigger
}

func (c *Cache) notify() {
	c.mu.RLock()
	trigger := c.trigger
	c.mu.RUnlock()
	if trigger != nil {
		trigger()
	}
}

func (c *Cache) Close() {
	c.mu.Lock()
	watchers := c.watchers
	c.watchers = make(map[schema.GroupVersionKind]*inputWatcher)
	for _, watcher := range watchers {
		watcher.cancel()
	}
	c.mu.Unlock()
	for _, watcher := range watchers {
		<-watcher.done
	}
}

type inputWatcher struct {
	kind     Kind
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	informer cache.SharedIndexInformer

	mu       sync.RWMutex
	err      error
	watching bool
}

func (c *Cache) newWatcher(parent context.Context, kind Kind) (*inputWatcher, error) {
	ctx, cancel := context.WithCancel(parent)
	w := &inputWatcher{kind: kind, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	resource := c.client.Resource(kind.Resource)
	setHealth := func(err error, watching bool) {
		w.mu.Lock()
		w.err, w.watching = err, watching
		w.mu.Unlock()
		c.notify()
	}
	lw := &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			result, err := resource.List(ctx, options)
			if err != nil {
				setHealth(err, false)
			}
			return result, err
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			result, err := resource.Watch(ctx, options)
			setHealth(err, err == nil)
			return result, err
		},
	}
	w.informer = cache.NewSharedIndexInformer(lw, &unstructured.Unstructured{}, 0, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	_, err := w.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { c.notify() },
		UpdateFunc: func(any, any) { c.notify() },
		DeleteFunc: func(any) { c.notify() },
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("input kind %s event handler: %w", kind.GVK, err)
	}
	if err := w.informer.SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
		if ctx.Err() == nil {
			log.FromContext(ctx).Error(err, "Input list/watch failed", "kind", kind.GVK.String())
			setHealth(err, false)
		}
	}); err != nil {
		cancel()
		return nil, fmt.Errorf("input kind %s error handler: %w", kind.GVK, err)
	}
	return w, nil
}
