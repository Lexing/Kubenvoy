package kubenvoy

import (
	"fmt"
	"io/ioutil"
	"kubenvoy/utils"
	"strconv"
	"sync"

	"github.com/golang/glog"

	envoy "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	envoyCore "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	envoyEndpoint "github.com/envoyproxy/go-control-plane/envoy/api/v2/endpoint"
	"github.com/gogo/protobuf/proto"
	"github.com/gogo/protobuf/types"
	"github.com/google/uuid"
	"k8s.io/api/core/v1"

	"github.com/fsnotify/fsnotify"
)

// Not supported unfortunately ..... for now
func marshalAnyDeterministic(pb proto.Message) (*types.Any, error) {
	const googleApis = "type.googleapis.com/"
	b := make([]byte, 0, 1024*10)
	buffer := proto.NewBuffer(b)
	buffer.SetDeterministic(true)
	if err := buffer.Marshal(pb); err != nil {
		return nil, err
	}
	return &types.Any{TypeUrl: googleApis + proto.MessageName(pb), Value: buffer.Bytes()}, nil
}

func toAnySlice(messages []proto.Message) ([]types.Any, error) {
	results := make([]types.Any, 0)
	for _, m := range messages {
		any, err := types.MarshalAny(m)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal message for %v: %v", m, err)
		}
		results = append(results, *any)
	}

	return results, nil
}

func buildDiscoveryResponseOne(typeURL string, message proto.Message) (*envoy.DiscoveryResponse, error) {
	anys, err := toAnySlice([]proto.Message{message})
	if err != nil {
		return nil, fmt.Errorf("failed to build discovery response: %v", err)
	}
	resp := &envoy.DiscoveryResponse{
		TypeUrl:   typeURL,
		Resources: anys,
	}
	return resp, nil
}

func buildDiscoveryResponseSlice(typeURL string, messages []proto.Message) (*envoy.DiscoveryResponse, error) {
	anys, err := toAnySlice(messages)
	if err != nil {
		return nil, fmt.Errorf("failed to build discovery response: %v", err)
	}
	resp := &envoy.DiscoveryResponse{
		TypeUrl:   typeURL,
		Resources: anys,
	}
	return resp, nil
}

func addVersionAndNonce(resp *envoy.DiscoveryResponse) error {
	// // Calculate and add fingerprint
	fp, err := utils.ProtoFingerprint(resp)
	if err != nil {
		return fmt.Errorf("failed to generate fingerprint for resp %v. this should not happen", resp)
	}

	// Add fp as version info
	resp.VersionInfo = strconv.FormatUint(fp, 36)

	// Add uuid as nonce
	resp.Nonce = uuid.New().String()
	return nil
}

// BuildDiscoveryResponseOne creates DiscoveryResponse with given typeURL and one proto Message
func BuildDiscoveryResponseOne(typeURL string, msg proto.Message) (*envoy.DiscoveryResponse, error) {
	resp, err := buildDiscoveryResponseOne(typeURL, msg)
	if err != nil {
		return nil, fmt.Errorf("failed to generate discovery response, skip\n error: %v", err)
	}

	if err := addVersionAndNonce(resp); err != nil {
		return nil, fmt.Errorf("failed to add version and nonce info to response: %v", err)
	}
	return resp, nil
}

// BuildDiscoveryResponseSlice creates DiscoveryResponse with given typeURL and many proto Messages
func BuildDiscoveryResponseSlice(typeURL string, msgs []proto.Message) (*envoy.DiscoveryResponse, error) {
	resp, err := buildDiscoveryResponseSlice(typeURL, msgs)
	if err != nil {
		return nil, fmt.Errorf("failed to generate discovery response, skip\n error: %v", err)
	}

	if err := addVersionAndNonce(resp); err != nil {
		return nil, fmt.Errorf("failed to add version and nonce info to response: %v", err)
	}
	return resp, nil
}

func portAvailable(targetPort uint32, ports []v1.EndpointPort) bool {
	for _, p := range ports {
		if targetPort == uint32(p.Port) {
			return true
		}
	}

	return false
}

// ClusterLoadAssignmentFromEndpoint converts k8s API object endpoints to envoy api ClusterLoadAssignment
func ClusterLoadAssignmentFromEndpoint(endpoints *v1.Endpoints, targetPort uint32, originalPort int) *envoy.ClusterLoadAssignment {
	type Address = envoyCore.Address
	type SocketAddress = envoyCore.SocketAddress
	type Address_SocketAddress = envoyCore.Address_SocketAddress
	type SocketAddress_PortValue = envoyCore.SocketAddress_PortValue

	name := endpoints.GetObjectMeta().GetName()
	namespace := endpoints.GetObjectMeta().GetNamespace()
	clusterName := kubenvoyTargetPrefix + fmt.Sprintf("%v.%v:%v", name, namespace, originalPort)

	if endpoints == nil {
		return &envoy.ClusterLoadAssignment{
			ClusterName: clusterName,
			Endpoints:   []envoyEndpoint.LocalityLbEndpoints{},
		}
	}

	lbendpoints := []envoyEndpoint.LbEndpoint{}
	for _, subset := range endpoints.Subsets {
		if !portAvailable(targetPort, subset.Ports) {
			glog.V(0).Infof("target port %v not found in endpoints for %s", name, targetPort)
			continue
		}

		for _, address := range subset.Addresses {
			lbendpoints = append(lbendpoints, envoyEndpoint.LbEndpoint{
				Endpoint: &envoyEndpoint.Endpoint{
					Address: &Address{Address: &Address_SocketAddress{&SocketAddress{
						Address:       address.IP,
						PortSpecifier: &envoyCore.SocketAddress_PortValue{targetPort},
					},
					}},
				},
			})
		}
	}

	return &envoy.ClusterLoadAssignment{
		ClusterName: clusterName,
		Endpoints: []envoyEndpoint.LocalityLbEndpoints{
			envoyEndpoint.LocalityLbEndpoints{
				LbEndpoints: lbendpoints,
			},
		},
	}
}

// BuildEDSResponse builds an envoy EDS DiscoveryResponse with given endpoints and port
func BuildEDSResponse(endpoints *v1.Endpoints, targetPort uint32, originalPort int) (*envoy.DiscoveryResponse, error) {
	assignment := ClusterLoadAssignmentFromEndpoint(endpoints, targetPort, originalPort)
	return BuildDiscoveryResponseOne("type.googleapis.com/envoy.api.v2.ClusterLoadAssignment", assignment)
}

type serviceSlice []*v1.Service

func (slice serviceSlice) ToEnvoyClusters() []*envoy.Cluster {
	clusters := make([]*envoy.Cluster, 0)
	for _, service := range slice {
		clusters = append(clusters, clustersFromOneService(service)...)
	}
	return clusters
}

func clustersFromOneService(svc *v1.Service) []*envoy.Cluster {
	clusters := []*envoy.Cluster{}

	clusterConfig := svc.GetAnnotations()["kubenvoy.Cluster"]
	configOverride := &envoy.Cluster{}
	if clusterConfig != "" {
		var err error
		configOverride, err = yamlToEnvoyCluster([]byte(clusterConfig))
		if err != nil {
			glog.Errorf("failed to parse cluster config in svc annotation %v", err)
		}
	}

	for _, p := range svc.Spec.Ports {
		clusterName := kubenvoyTargetPrefix + fmt.Sprintf("%v.%v:%v", svc.Name, svc.Namespace, p.Port)
		c := proto.Clone(defaultCluster).(*envoy.Cluster)
		proto.Merge(c, configOverride)
		c.Name = clusterName
		clusters = append(clusters, c)
	}

	return clusters
}

// BuildCDSResponse builds a envoy CDS response with a slice of k8s services.
func BuildCDSResponse(slice serviceSlice) (*envoy.DiscoveryResponse, error) {
	clusters := slice.ToEnvoyClusters()
	msgs := make([]proto.Message, len(clusters))
	for i, c := range clusters {
		msgs[i] = c
	}
	return BuildDiscoveryResponseSlice("type.googleapis.com/envoy.api.v2.Cluster", msgs)
}

// BuildLDSResponse builds a envoy LDS response with a slice of k8s services.
func BuildLDSResponse(listeners []envoy.Listener) (*envoy.DiscoveryResponse, error) {
	msgs := make([]proto.Message, len(listeners))
	for i, listener := range listeners {
		msgs[i] = &listener
	}
	return BuildDiscoveryResponseSlice("type.googleapis.com/envoy.api.v2.Listener", msgs)
}

type EnvoyListenerConfigWatcher struct {
	path      string
	listeners []envoy.Listener

	// file path to watch
	filewatcher *fsnotify.Watcher

	stopChan chan struct{}

	handlers []ListenerHandler
	mutex    sync.RWMutex
}

func NewEnvoyListenerConfigWatcher(path string) *EnvoyListenerConfigWatcher {
	w := &EnvoyListenerConfigWatcher{
		path:     path,
		stopChan: make(chan struct{}),
	}

	var err error
	w.filewatcher, err = fsnotify.NewWatcher()
	if err != nil {
		glog.Fatalf("failed to start watcher %v", err)
	}

	if err := w.Start(); err != nil {
		glog.Fatalf("failed to watch envoy listener config %v", err)
	}

	w.filewatcher.Add(path)
	return w
}

type ListenerHandler func([]envoy.Listener)

func (m *EnvoyListenerConfigWatcher) AddHandler(h ListenerHandler) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	h(m.listeners)
	m.handlers = append(m.handlers, h)
}

func (m *EnvoyListenerConfigWatcher) loadConfig() error {
	data, err := ioutil.ReadFile(m.path)
	if err != nil {
		return fmt.Errorf("cannot read listeners config file: %v", err)
	}

	listeners, err := yamlToEnvoyListener(data)
	if err != nil {
		return fmt.Errorf("failed to load listeners config file: %v", err)
	}

	m.mutex.Lock()
	m.listeners = listeners
	m.mutex.Unlock()
	return nil
}

func (m *EnvoyListenerConfigWatcher) handle() {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	for _, h := range m.handlers {
		h(m.listeners)
	}
}

func (m *EnvoyListenerConfigWatcher) loadAndHandle() error {
	if err := m.loadConfig(); err != nil {
		return fmt.Errorf("failed to load config %v", err)
	}

	m.handle()
	return nil
}

func (m *EnvoyListenerConfigWatcher) Start() error {
	if err := m.loadAndHandle(); err != nil {
		return err
	}

	go func() {
		for {
			select {
			case event, ok := <-m.filewatcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Remove == fsnotify.Remove {
					glog.V(0).Info("Listener config file updated.")
					m.filewatcher.Remove(event.Name)
					m.filewatcher.Add(event.Name)
					m.loadAndHandle()
				}
			case err, ok := <-m.filewatcher.Errors:
				if !ok {
					return
				}
				glog.Errorf("filewatcher error:", err)
			}
		}
	}()

	return nil
}

func (m *EnvoyListenerConfigWatcher) Close() {
	close(m.stopChan)
	m.filewatcher.Close()
}
