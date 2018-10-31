package kubenvoy

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

type SharedInformerWrapper struct {
	started      uint32
	startedMutex sync.Mutex

	stopChan chan struct{}
	cache.SharedInformer
}

func NewSharedInformerWrapper(informer cache.SharedInformer, stopChan chan struct{}) *SharedInformerWrapper {
	return &SharedInformerWrapper{
		SharedInformer: informer,
		stopChan:       stopChan,
	}
}

// RunOnce starts original SharedInformer only once, it's safe to call this function
// multiple times.
func (sw *SharedInformerWrapper) RunOnce() {
	if atomic.LoadUint32(&sw.started) == 1 {
		return
	}

	sw.startedMutex.Lock()

	defer sw.startedMutex.Unlock()
	if sw.started == 0 {
		// This is a little bit different than sync.Once, that we
		// set the number to 1 before actually calling the function.
		// other calls won't blocked before the first one finishes
		// because of the change.
		atomic.StoreUint32(&sw.started, 1)
		sw.Run(sw.stopChan)
	}
}

type K8SResourceWatcher struct {
	k8sClientSet *kubernetes.Clientset

	// map from target to watch instance
	watches map[string]*SharedInformerWrapper
	mutex   *sync.RWMutex
}

func NewK8EndpointsWatcher(clientset *kubernetes.Clientset) *K8SResourceWatcher {
	return &K8SResourceWatcher{
		k8sClientSet: clientset,
		watches:      make(map[string]*SharedInformerWrapper),
		mutex:        &sync.RWMutex{},
	}
}

// targetKey generates a key for obj
func targetKey(resourceName string, namespace string, selector fields.Selector) string {
	return fmt.Sprintf("%v_%v_%v", resourceName, namespace, selector)
}

// WatchEndpoints watches specified target and then process the events with handler. It blocks until
// context is cancelled.
func (w *K8SResourceWatcher) watchResource(namespace string, fieldSelector fields.Selector, labelSelector labels.Selector, objType runtime.Object, resourceName string, stopChan chan struct{}) cache.SharedInformer {
	key := targetKey(resourceName, namespace, fieldSelector)
	w.mutex.Lock()
	_, exist := w.watches[key]
	if !exist {
		fieldSelectorStr := ""
		if fieldSelector != nil {
			fieldSelectorStr = fieldSelector.String()
		}
		labelSelectorStr := ""
		if labelSelector != nil {
			labelSelectorStr = labelSelector.String()
		}

		optionsModifier := func(options *metav1.ListOptions) {
			options.FieldSelector = fieldSelectorStr
			options.LabelSelector = labelSelectorStr
		}

		lw := cache.NewFilteredListWatchFromClient(w.k8sClientSet.CoreV1().RESTClient(), resourceName, namespace, optionsModifier)
		tmp := cache.NewSharedInformer(lw, objType, time.Minute*10) // default resync timer
		sw := NewSharedInformerWrapper(tmp, stopChan)
		w.watches[key] = sw
	}
	sw := w.watches[key]
	w.mutex.Unlock()

	go sw.RunOnce()
	return sw
}

func (w *K8SResourceWatcher) WatchEndpoints(namespace string, name string, handler EndpointsHandler, stopChan chan struct{}) {
	fieldSelector := fields.OneTermEqualSelector("metadata.name", name)
	sw := w.watchResource(namespace, fieldSelector, nil, &v1.Endpoints{}, "endpoints", stopChan)
	sw.AddEventHandler(handler.EndpointsHandlerFuncs())
}

func (w *K8SResourceWatcher) WatchServices(namespace string, labelSelector labels.Selector, handler ServicesHandler, stopChan chan struct{}) {
	sw := w.watchResource(namespace, nil, labelSelector, &v1.Service{}, "services", stopChan)
	sw.AddEventHandler(NewSharedInformerResourceEventHandlerWrapper(sw, handler, true))
}
