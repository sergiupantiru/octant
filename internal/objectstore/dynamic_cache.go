package objectstore

import (
	"context"
	"sync"
	"time"

	"github.com/heptio/developer-dash/internal/cluster"
	"github.com/heptio/developer-dash/internal/log"
	"github.com/heptio/developer-dash/internal/util/retry"
	"github.com/heptio/developer-dash/pkg/objectstoreutil"
	"github.com/heptio/developer-dash/third_party/k8s.io/client-go/dynamic/dynamicinformer"
	"github.com/pkg/errors"
	"go.opencensus.io/trace"
	authorizationv1 "k8s.io/api/authorization/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kLabels "k8s.io/apimachinery/pkg/labels"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/informers"
	kcache "k8s.io/client-go/tools/cache"
)

const (
	// defaultMutableResync is the resync period for informers.
	defaultInformerResync = time.Second * 180
)

func initDynamicSharedInformerFactory(client cluster.ClientInterface, namespace string) (dynamicinformer.DynamicSharedInformerFactory, error) {
	dynamicClient, err := client.DynamicClient()
	if err != nil {
		return nil, err
	}
	if namespace == "" {
		return dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, defaultInformerResync), nil
	}
	return dynamicinformer.NewFilteredDynamicSharedInformerFactory(dynamicClient, defaultInformerResync, namespace, nil), nil
}

func currentInformer(
	gvr schema.GroupVersionResource,
	factory dynamicinformer.DynamicSharedInformerFactory,
	stopCh <-chan struct{}) (informers.GenericInformer, error) {
	if factory == nil {
		return nil, errors.New("dynamic shared informer factory is nil")
	}

	informer := factory.ForResource(gvr)
	factory.Start(stopCh)

	return informer, nil
}

type accessKey struct {
	Namespace string
	Group     string
	Resource  string
	Verb      string
}
type accessMap map[accessKey]bool

// DynamicCacheOpt is an option for configuration DynamicCache.
type DynamicCacheOpt func(*DynamicCache)

// DynamicCache is a cache based on the dynamic shared informer factory.
type DynamicCache struct {
	initFactoryFunc func(cluster.ClientInterface, string) (dynamicinformer.DynamicSharedInformerFactory, error)
	factories       map[string]dynamicinformer.DynamicSharedInformerFactory
	client          cluster.ClientInterface
	stopCh          <-chan struct{}
	seenGVKs        map[string]map[schema.GroupVersionKind]bool
	access          accessMap

	mu sync.Mutex
}

var _ (ObjectStore) = (*DynamicCache)(nil)

// NewDynamicCache creates an instance of DynamicCache.
func NewDynamicCache(client cluster.ClientInterface, stopCh <-chan struct{}, options ...DynamicCacheOpt) (*DynamicCache, error) {

	c := &DynamicCache{
		initFactoryFunc: initDynamicSharedInformerFactory,
		client:          client,
		stopCh:          stopCh,
		seenGVKs:        make(map[string]map[schema.GroupVersionKind]bool),
	}

	for _, option := range options {
		option(c)
	}

	if c.access == nil {
		c.access = make(accessMap)
	}

	namespaceClient, err := client.NamespaceClient()
	if err != nil {
		return nil, errors.Wrap(err, "client namespace")
	}

	namespaces, err := namespaceClient.Names()
	if err != nil {
		namespaces = []string{namespaceClient.InitialNamespace()}
	}

	namespaces = append(namespaces, "")

	factories := make(map[string]dynamicinformer.DynamicSharedInformerFactory)
	for _, namespace := range namespaces {
		factory, err := c.initFactoryFunc(client, namespace)
		if err != nil {
			return nil, errors.Wrap(err, "initialize dynamic shared informer factory")
		}
		factories[namespace] = factory
		c.seenGVKs[namespace] = make(map[schema.GroupVersionKind]bool)
	}
	c.factories = factories
	return c, nil
}

type lister interface {
	List(selector kLabels.Selector) ([]kruntime.Object, error)
}

func (dc *DynamicCache) fetchAccess(key accessKey, verb string) (bool, error) {
	k8sClient, err := dc.client.KubernetesClient()
	if err != nil {
		return false, errors.Wrap(err, "client kubernetes")
	}

	authClient := k8sClient.AuthorizationV1()
	sar := &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: key.Namespace,
				Group:     key.Group,
				Resource:  key.Resource,
				Verb:      verb,
			},
		},
	}

	rresponse, err := authClient.SelfSubjectAccessReviews().Create(sar)
	if err != nil {
		return false, errors.Wrap(err, "client auth")
	}
	return rresponse.Status.Allowed, nil
}

// HasAccess returns an error if the current user does not have access to perform the verb action
// for the given key.
func (dc *DynamicCache) HasAccess(key objectstoreutil.Key, verb string) error {
	gvk := key.GroupVersionKind()
	gvr, err := dc.client.Resource(gvk.GroupKind())
	if err != nil {
		return errors.Wrap(err, "client resource")
	}

	aKey := accessKey{
		Namespace: key.Namespace,
		Group:     gvr.Group,
		Resource:  gvr.Resource,
		Verb:      verb,
	}

	access, ok := dc.access[aKey]
	if !ok {
		val, err := dc.fetchAccess(aKey, verb)
		if err != nil {
			return errors.Wrapf(err, "fetch access: %+v", aKey)
		}

		dc.mu.Lock()
		dc.access[aKey] = val
		dc.mu.Unlock()

		access, ok = dc.access[aKey]
		if !ok {
			return errors.Errorf("unknown key: %+v", aKey)
		}
	}

	if !access {
		return errors.Errorf("denied %+v", aKey)
	}

	return nil
}

func (dc *DynamicCache) currentInformer(key objectstoreutil.Key) (informers.GenericInformer, error) {
	if dc.client == nil {
		return nil, errors.New("cluster client is nil")
	}

	gvk := key.GroupVersionKind()
	gvr, err := dc.client.Resource(gvk.GroupKind())
	if err != nil {
		return nil, errors.Wrap(err, "client resource")
	}

	factory, ok := dc.factories[key.Namespace]
	if !ok {
		return nil, errors.Errorf("no informer factory for namespace %s", key.Namespace)
	}

	informer, err := currentInformer(gvr, factory, dc.stopCh)
	if err != nil {
		return nil, err
	}

	dc.mu.Lock()
	defer dc.mu.Unlock()

	if _, ok := dc.seenGVKs[key.Namespace][gvk]; ok {
		return informer, nil
	}

	ctx := context.Background()
	if !kcache.WaitForCacheSync(ctx.Done(), informer.Informer().HasSynced) {
		return nil, errors.New("shutting down")
	}

	dc.seenGVKs[key.Namespace][gvk] = true

	return informer, nil
}

// List lists objects.
func (dc *DynamicCache) List(ctx context.Context, key objectstoreutil.Key) ([]*unstructured.Unstructured, error) {
	_, span := trace.StartSpan(ctx, "dynamicCacheList")
	defer span.End()

	if err := dc.HasAccess(key, "list"); err != nil {
		return nil, errors.Wrapf(err, "list access forbidden to %+v", key)
	}

	span.Annotate([]trace.Attribute{
		trace.StringAttribute("namespace", key.Namespace),
		trace.StringAttribute("apiVersion", key.APIVersion),
		trace.StringAttribute("kind", key.Kind),
	}, "list key")

	informer, err := dc.currentInformer(key)
	if err != nil {
		return nil, errors.Wrapf(err, "retrieving informer for %+v", key)
	}

	var l lister
	if key.Namespace == "" {
		l = informer.Lister()
	} else {
		l = informer.Lister().ByNamespace(key.Namespace)
	}

	var selector = kLabels.Everything()
	if key.Selector != nil {
		selector = key.Selector.AsSelector()
	}

	objects, err := l.List(selector)
	if err != nil {
		return nil, errors.Wrapf(err, "listing %v", key)
	}

	list := make([]*unstructured.Unstructured, len(objects))
	for i, obj := range objects {
		u, err := kruntime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			return nil, errors.Wrapf(err, "converting %T to unstructured", obj)
		}
		list[i] = &unstructured.Unstructured{Object: u}
	}

	return list, nil
}

type getter interface {
	Get(string) (kruntime.Object, error)
}

// Get retrieves a single object.
func (dc *DynamicCache) Get(ctx context.Context, key objectstoreutil.Key) (*unstructured.Unstructured, error) {
	_, span := trace.StartSpan(ctx, "dynamicCacheList")
	defer span.End()

	if err := dc.HasAccess(key, "get"); err != nil {
		return nil, errors.Wrapf(err, "get access forbidden to %+v", key)
	}

	span.Annotate([]trace.Attribute{
		trace.StringAttribute("namespace", key.Namespace),
		trace.StringAttribute("apiVersion", key.APIVersion),
		trace.StringAttribute("kind", key.Kind),
		trace.StringAttribute("name", key.Name),
	}, "get key")

	informer, err := dc.currentInformer(key)
	if err != nil {
		return nil, errors.Wrapf(err, "retrieving informer for %v", key)
	}

	var g getter
	if key.Namespace == "" {
		g = informer.Lister()
	} else {
		g = informer.Lister().ByNamespace(key.Namespace)
	}

	var retryCount int64

	var object kruntime.Object
	retryErr := retry.Retry(3, time.Second, func() error {
		object, err = g.Get(key.Name)
		if err != nil {
			if !kerrors.IsNotFound(err) {
				retryCount++
				return retry.Stop(errors.Wrap(err, "lister Get"))
			}
			return err
		}

		return nil
	})

	if retryCount > 0 {
		span.Annotate([]trace.Attribute{
			trace.Int64Attribute("retryCount", retryCount),
		}, "get retried")
	}

	if retryErr != nil {
		return nil, err
	}

	// Verify the selector matches if provided
	if key.Selector != nil {
		accessor := meta.NewAccessor()
		m, err := accessor.Labels(object)
		if err != nil {
			return nil, errors.New("retrieving labels")
		}
		labels := kLabels.Set(m)
		selector := key.Selector.AsSelector()
		if !selector.Matches(labels) {
			return nil, errors.New("object found but filtered by selector")
		}
	}

	u, err := kruntime.DefaultUnstructuredConverter.ToUnstructured(object)
	if err != nil {
		return nil, errors.Wrapf(err, "converting %T to unstructured", object)
	}
	return &unstructured.Unstructured{Object: u}, nil
}

// Watch watches the cluster for an event and performs actions with the
// supplied handler.
func (dc *DynamicCache) Watch(ctx context.Context, key objectstoreutil.Key, handler kcache.ResourceEventHandler) error {
	logger := log.From(ctx)
	if err := dc.HasAccess(key, "watch"); err != nil {
		logger.Errorf("check access failed: %v, access forbidden to %+v", key)
		return nil
	}

	informer, err := dc.currentInformer(key)
	if err != nil {
		return errors.Wrapf(err, "retrieving informer for %s", key)
	}

	informer.Informer().AddEventHandler(handler)
	return nil
}