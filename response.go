package kubenvoyxds

import (
	"flag"
	"fmt"
	"io/ioutil"
	"kubenvoy/utils"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"

	envoy "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	envoyCore "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	envoyEndpoint "github.com/envoyproxy/go-control-plane/envoy/api/v2/endpoint"
	envoyBootstrap "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v2"
	"github.com/gogo/protobuf/proto"
	"github.com/gogo/protobuf/types"
	"github.com/google/uuid"
	"k8s.io/api/core/v1"

	"github.com/fsnotify/fsnotify"
	"github.com/ghodss/yaml"
	"github.com/gogo/protobuf/jsonpb"
)

var (
	edsClusterName = flag.String("eds_cluster_name", "xds_cluster", "XDS cluster name.")
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
func ClusterLoadAssignmentFromEndpoint(endpoints *v1.Endpoints, targetPort uint32) *envoy.ClusterLoadAssignment {
	type Address = envoyCore.Address
	type SocketAddress = envoyCore.SocketAddress
	type Address_SocketAddress = envoyCore.Address_SocketAddress
	type SocketAddress_PortValue = envoyCore.SocketAddress_PortValue

	name := endpoints.GetObjectMeta().GetName()

	if endpoints == nil {
		return &envoy.ClusterLoadAssignment{
			ClusterName: kubenvoyTargetPrefix + fmt.Sprintf("%v:%v", name, targetPort),
			Endpoints:   []envoyEndpoint.LocalityLbEndpoints{},
		}
	}

	lbendpoints := []envoyEndpoint.LbEndpoint{}
	for _, subset := range endpoints.Subsets {
		if !portAvailable(targetPort, subset.Ports) {
			glog.V(0).Infof("target port not found in endpoints for %s", name)
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
		ClusterName: name,
		Endpoints: []envoyEndpoint.LocalityLbEndpoints{
			envoyEndpoint.LocalityLbEndpoints{
				LbEndpoints: lbendpoints,
			},
		},
	}
}

// BuildEDSResponse builds an envoy EDS DiscoveryResponse with given endpoints and port
func BuildEDSResponse(endpoints *v1.Endpoints, port uint32) (*envoy.DiscoveryResponse, error) {
	assignment := ClusterLoadAssignmentFromEndpoint(endpoints, port)
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
	for _, p := range svc.Spec.Ports {
		c := &envoy.Cluster{
			Name:                 kubenvoyTargetPrefix + fmt.Sprintf("%v.%v:%v", svc.Name, svc.Namespace, p.Port),
			ConnectTimeout:       time.Second * 1,
			LbPolicy:             envoy.Cluster_ROUND_ROBIN,
			Type:                 envoy.Cluster_EDS,
			Http2ProtocolOptions: &envoyCore.Http2ProtocolOptions{},
			EdsClusterConfig: &envoy.Cluster_EdsClusterConfig{
				EdsConfig: &envoyCore.ConfigSource{
					ConfigSourceSpecifier: &envoyCore.ConfigSource_ApiConfigSource{ApiConfigSource: &envoyCore.ApiConfigSource{
						ApiType: envoyCore.ApiConfigSource_GRPC,
						GrpcServices: []*envoyCore.GrpcService{
							&envoyCore.GrpcService{
								TargetSpecifier: &envoyCore.GrpcService_EnvoyGrpc_{
									EnvoyGrpc: &envoyCore.GrpcService_EnvoyGrpc{
										ClusterName: *edsClusterName,
									},
								}},
						},
					},
					},
				},
			},
		}

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

func envoyListenerFromYAML(data []byte) ([]envoy.Listener, error) {
	data, err := yaml.YAMLToJSON(data)
	if err != nil {
		return nil, fmt.Errorf("cannot parse YAML file to JSON file: %v", err)
	}
	glog.Errorf(string(data))
	resources := envoyBootstrap.Bootstrap_StaticResources{}
	if err := jsonpb.Unmarshal(strings.NewReader(string(data)), &resources); err != nil {
		return nil, fmt.Errorf("failed to unmarshal listeners %v", err)
	}

	return resources.GetListeners(), nil
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

	listeners, err := envoyListenerFromYAML(data)
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
		// resync period pass as param
		ticker := time.NewTicker(time.Second * 60)
		for {
			select {
			case <-ticker.C:
				if err := m.loadAndHandle(); err != nil {
					glog.Error(err)
				}
			case <-m.stopChan:
				return
			}
		}
	}()

	go func() {
		for {
			select {
			case event, ok := <-m.filewatcher.Events:
				if !ok {
					return
				}
				glog.Error("event:", event)
				if event.Op&fsnotify.Remove == fsnotify.Remove {
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
