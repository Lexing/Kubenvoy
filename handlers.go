package kubenvoy

import (
	"sort"
	"strings"
	"time"

	"github.com/golang/glog"
	"k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
)

// This is a bit bad, maybe use Watch.interface ?

func handleEventHandlerPanic(f func()) {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("Panic in handling events: %v", r)
		}
	}()
	f()
}

// ResourcesHandler handles resources
type ResourcesHandler interface {
	Handle(resources []interface{})
}

// SharedInformerResourceEventHandlerWrapper implments cache.ResourceEventHandler,
// add triggers handle function seeing any event. It's capable of grouping
// results togethers with debounced function
type SharedInformerResourceEventHandlerWrapper struct {
	informer         cache.SharedInformer
	handler          ResourcesHandler
	debouncedTrigger func()
}

func NewSharedInformerResourceEventHandlerWrapper(informer cache.SharedInformer, handler ResourcesHandler, debounce bool) *SharedInformerResourceEventHandlerWrapper {
	wrapper := &SharedInformerResourceEventHandlerWrapper{
		informer: informer,
		handler:  handler,
	}

	if debounce {
		wrapper.debouncedTrigger = debounced(1000*time.Millisecond, wrapper.trigger)
	}

	return wrapper
}

func debounced(interval time.Duration, f func()) func() {

	in := make(chan func())
	out := make(chan func())

	go func() {
		var f func() = func() {}
		for {
			select {
			case f = <-in:
			case <-time.After(interval):
				out <- f
				<-in
				// new interval
			}
		}
	}()

	go func() {
		for {
			select {
			case f := <-out:
				f()
			}
		}
	}()

	return func() {
		in <- f
	}
}

func (w *SharedInformerResourceEventHandlerWrapper) trigger() {
	handleEventHandlerPanic(func() {
		w.handler.Handle(w.informer.GetStore().List())
	})
}

func (w *SharedInformerResourceEventHandlerWrapper) onEvent() {
	if w.debouncedTrigger != nil {
		w.debouncedTrigger()
	} else {
		w.trigger()
	}
}

// OnAdd implements cache.ResourceEventHandler
func (w *SharedInformerResourceEventHandlerWrapper) OnAdd(obj interface{}) {
	glog.V(3).Infof("OnAdd %v [%v] [%v]", obj, w.informer.LastSyncResourceVersion())
	w.onEvent()
}

// OnUpdate implements cache.ResourceEventHandler
func (w *SharedInformerResourceEventHandlerWrapper) OnUpdate(oldObj, newObj interface{}) {
	glog.V(3).Infof("OnUpdate %v [%v] [%v]", newObj, w.informer.LastSyncResourceVersion())
	w.onEvent()
}

// OnDelete implements cache.ResourceEventHandler
func (w *SharedInformerResourceEventHandlerWrapper) OnDelete(obj interface{}) {
	glog.V(3).Infof("OnDelete %v [%v] [%v] ", obj, w.informer.LastSyncResourceVersion())
	w.onEvent()
}

// EndpointsHandler handles one kubernetes endpoints result, it implements cache.ResourcesHandler
type EndpointsHandler func(endpoints *v1.Endpoints)

func (h EndpointsHandler) EndpointsHandlerFuncs() *cache.ResourceEventHandlerFuncs {
	return &cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			h(obj.(*v1.Endpoints))
		},

		UpdateFunc: func(obj, newObj interface{}) {
			h(obj.(*v1.Endpoints))
		},

		DeleteFunc: func(obj interface{}) {
			h(nil)
		},
	}
}

// ServicesHandler handles kubernetes services results, it implements ResourcesHandler
type ServicesHandler func(service []*v1.Service)

func (h ServicesHandler) Handle(resources []interface{}) {
	services := make(ServiceSorter, len(resources))
	for i, r := range resources {
		services[i] = r.(*v1.Service)
	}
	sort.Sort(services)
	h(services)
}

type ServiceSorter []*v1.Service

// Len is part of sort.Interface.
func (s ServiceSorter) Len() int {
	return len(s)
}

// Swap is part of sort.Interface.
func (s ServiceSorter) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

// Less is part of sort.Interface. It is implemented by calling the "by" closure in the sorter.
func (s ServiceSorter) Less(i, j int) bool {
	namespaceCompare := strings.Compare(s[i].Namespace, s[j].Namespace)
	if namespaceCompare == 0 {
		nameCompare := strings.Compare(s[i].Name, s[j].Name)
		return nameCompare < 0
	}
	return namespaceCompare < 0
}
