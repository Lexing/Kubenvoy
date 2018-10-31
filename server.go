package kubenvoy

import (
	"context"
	"fmt"
	"io"
	"kubenvoy/utils"
	"strconv"
	"strings"

	"github.com/golang/glog"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	envoy "github.com/envoyproxy/go-control-plane/envoy/api/v2"
)

const kubenvoyTargetPrefix = "kubenvoy://"

type KubenvoyXDSServer struct {
	k8sClientSet          *kubernetes.Clientset
	watcher               *K8SResourceWatcher
	listenerConfigWatcher *EnvoyListenerConfigWatcher
}

// NewK8sClientSet creates new K8sClientSet with given masterURL & kubeConfigPath
func NewK8sClientSet(masterURL string, kubeConfigPath string) (*kubernetes.Clientset, error) {
	// creates the connection
	config, err := clientcmd.BuildConfigFromFlags(masterURL, kubeConfigPath)
	if err != nil {
		return nil, err
	}

	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return clientset, nil
}

func NewKubenvoyXDSServer(masterURL string, kubeConfigPath string) *KubenvoyXDSServer {
	clientset, err := NewK8sClientSet(masterURL, kubeConfigPath)
	if err != nil {
		glog.Fatal(err)
	}

	server := &KubenvoyXDSServer{
		k8sClientSet: clientset,
		watcher:      NewK8EndpointsWatcher(clientset),
	}

	return server
}

func NewGRPCKubenvoyXDSServer(masterURL string, kubeConfigPath string) *grpc.Server {
	s := NewKubenvoyXDSServer(masterURL, kubeConfigPath)
	rpcs := grpc.NewServer()
	envoy.RegisterEndpointDiscoveryServiceServer(rpcs, s)
	envoy.RegisterClusterDiscoveryServiceServer(rpcs, s)
	envoy.RegisterListenerDiscoveryServiceServer(rpcs, s)

	s.listenerConfigWatcher = NewEnvoyListenerConfigWatcher("/etc/kubenvoy/listeners.yaml")
	utils.OnTerminate(func() {
		s.listenerConfigWatcher.Close()
	})

	return rpcs
}

func (s *KubenvoyXDSServer) CreateEndpointsEventHandler(r *envoy.DiscoveryRequest, port int, stream *XDSStream) EndpointsHandler {
	return func(endpoints *v1.Endpoints) {
		resp, err := BuildEDSResponse(endpoints, uint32(port))
		if err != nil {
			glog.Errorf("Failed to generate EDS response: %v", err)
			return
		}

		clientVersion := stream.AppliedVersion(r.GetTypeUrl(), strings.Join(r.GetResourceNames(), "|"))
		if resp.VersionInfo == clientVersion {
			glog.V(0).Infof("Built endpoint version %v for %v:%v is same as client %s, not sending anything", clientVersion, endpoints.GetObjectMeta().GetName(), port, r.GetNode())
			return
		}

		glog.V(2).Infof("New endpoints resp %v", endpoints)
		glog.V(0).Infof("Sending client %s new endpoints config %s ", r.GetNode(), resp)
		stream.Send(resp)
	}
}

func (s *KubenvoyXDSServer) CreateServicesHandler(r *envoy.DiscoveryRequest, stream *XDSStream) ServicesHandler {
	return func(services []*v1.Service) {
		resp, err := BuildCDSResponse(services)
		if err != nil {
			glog.Errorf("Failed to generate CDS response: %v", err)
			return
		}

		clientVersion := stream.AppliedVersion(r.GetTypeUrl(), strings.Join(r.GetResourceNames(), "|"))
		if resp.VersionInfo == clientVersion {
			glog.V(0).Infof("Built service version %v for clusters is same as client %v, not sending anything", clientVersion, r.GetNode())
			return
		}

		glog.V(2).Infof("New clusters resp %v", services)
		glog.V(0).Infof("Sending client %s new clusters config %s ", r.GetNode(), resp)
		stream.Send(resp)
	}
}

func (s *KubenvoyXDSServer) CreateListenerHandler(r *envoy.DiscoveryRequest, stream *XDSStream) ListenerHandler {
	return func(listeners []envoy.Listener) {
		resp, err := BuildLDSResponse(listeners)
		if err != nil {
			glog.Errorf("failed to build LDS response %v", err)
			return
		}

		clientVersion := stream.AppliedVersion(r.GetTypeUrl(), strings.Join(r.GetResourceNames(), "|"))
		if resp.VersionInfo == clientVersion {
			glog.V(0).Infof("Built listeners version %v is same as client %s, not sending anything", clientVersion, r.GetNode())
			return
		}

		glog.V(0).Infof("Sending client %s new listener config \n: %s ", r.GetNode(), listeners)
		stream.Send(resp)
	}
}

func (s *KubenvoyXDSServer) Stream(originalStream grpc.ServerStream) error {
	stream := NewXDSStream(originalStream)
	go stream.listen()

	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		glog.V(3).Infof("handle discovery request [%v]", req.GetResourceNames())
		switch req.GetTypeUrl() {
		case "type.googleapis.com/envoy.api.v2.ClusterLoadAssignment":
			err := s.handleEndpointsDiscoveryRequest(req, stream)
			if err != nil {
				return err
			}
		case "type.googleapis.com/envoy.api.v2.Cluster":
			s.handleClusterDiscoveryRequest(req, stream)
		case "type.googleapis.com/envoy.api.v2.Listener":
			s.handleListenerDiscoveryRequest(req, stream)
		// case "type.googleapis.com/envoy.api.v2.RouteConfiguration":
		// s.handleClusterDiscoveryRequest(req, stream)
		default:
			glog.Errorf("Got unsupported req type", req)
		}
		glog.V(3).Infof("handle discovery request done [%v]", req.GetResourceNames())
	}
}

// StreamListeners implements one method of GRPC Envoy XDS service.
func (s *KubenvoyXDSServer) StreamListeners(stream envoy.ListenerDiscoveryService_StreamListenersServer) error {
	return s.Stream(stream)
}

// FetchListeners not implemented
func (s *KubenvoyXDSServer) FetchListeners(ctx context.Context, r *envoy.DiscoveryRequest) (*envoy.DiscoveryResponse, error) {
	glog.Errorf("Unsupported FetchListeners requests %v", r)
	return nil, grpc.Errorf(codes.Unimplemented, "")
}

// StreamRoutes implements one method of GRPC Envoy XDS service.
func (s *KubenvoyXDSServer) StreamRoutes(stream envoy.RouteDiscoveryService_StreamRoutesServer) error {
	return s.Stream(stream)
}

func (s *KubenvoyXDSServer) IncrementalRoutes(stream envoy.RouteDiscoveryService_IncrementalRoutesServer) error {
	glog.Errorf("Unsupported IncrementalRoutes requests %v", stream)
	return grpc.Errorf(codes.Unimplemented, "")
}

// FetchRoutes not implemented
func (s *KubenvoyXDSServer) FetchRoutes(ctx context.Context, r *envoy.DiscoveryRequest) (*envoy.DiscoveryResponse, error) {
	glog.Errorf("Unsupported FetchRoutes requests %v", r)
	return nil, grpc.Errorf(codes.Unimplemented, "")
}

// StreamEndpoints implements one method of GRPC Envoy XDS service.
func (s *KubenvoyXDSServer) StreamEndpoints(endpointStream envoy.EndpointDiscoveryService_StreamEndpointsServer) error {
	return s.Stream(endpointStream)
}

// FetchEndpoints not implemented
func (s *KubenvoyXDSServer) FetchEndpoints(ctx context.Context, r *envoy.DiscoveryRequest) (*envoy.DiscoveryResponse, error) {
	glog.Errorf("Unsupported FetchEndpoints requests %v", r)
	return nil, grpc.Errorf(codes.Unimplemented, "")
}

func (s *KubenvoyXDSServer) StreamClusters(clusterStream envoy.ClusterDiscoveryService_StreamClustersServer) error {
	return s.Stream(clusterStream)
}

func (s *KubenvoyXDSServer) IncrementalClusters(stream envoy.ClusterDiscoveryService_IncrementalClustersServer) error {
	glog.Errorf("Unsupported IncrementalClusters requests %v", stream)
	return grpc.Errorf(codes.Unimplemented, "")
}

func (s *KubenvoyXDSServer) FetchClusters(ctx context.Context, r *envoy.DiscoveryRequest) (*envoy.DiscoveryResponse, error) {
	glog.Errorf("Unsupported FetchClusters requests %v", r)
	return nil, grpc.Errorf(codes.Unimplemented, "")
}

func (s *KubenvoyXDSServer) handleEndpointsDiscoveryRequest(r *envoy.DiscoveryRequest, stream *XDSStream) error {
	glog.V(0).Infof("HandleEndpointsDiscoveryRequest [%v] %v", r.GetResourceNames(), r.GetResponseNonce())
	stopChan := utils.StopChanOnTerminate()
	for _, resourceName := range r.GetResourceNames() {
		target, svcPort, err := parseTargetResourceName(resourceName)
		if err != nil {
			err := fmt.Errorf("Failed to parse resource %v: %v. Skip", r, err)
			glog.Error(err)
			return err
		}

		svc, err := s.k8sClientSet.CoreV1().Services(target.Namespace).Get(target.Name, metav1.GetOptions{})
		if err != nil {
			err := fmt.Errorf("Failed to get svc %v from k8s", target, err)
			glog.Error(err)
			return err
		}

		var targetPort int
		for _, p := range svc.Spec.Ports {
			if p.Port == int32(svcPort) {
				targetPort = p.TargetPort.IntValue()
				break
			}
		}

		if targetPort == 0 {
			glog.Errorf("Failed to find target port for port %v in %v", svcPort, target)
			continue
		}

		handler := s.CreateEndpointsEventHandler(r, targetPort, stream)
		go s.watcher.WatchEndpoints(target.Namespace, target.Name, handler, stopChan)
	}

	return nil
}

func (s *KubenvoyXDSServer) handleClusterDiscoveryRequest(r *envoy.DiscoveryRequest, stream *XDSStream) {
	glog.V(0).Infof("HandleClusterDiscoveryRequest [%v] %v", r.GetResourceNames(), r.GetResponseNonce())
	stopChan := utils.StopChanOnTerminate()
	handler := s.CreateServicesHandler(r, stream)
	requirement, _ := labels.NewRequirement("kubenvoy-discovery", selection.Equals, []string{"true"})
	labelSelector := labels.NewSelector().Add(*requirement)
	go s.watcher.WatchServices(v1.NamespaceAll, labelSelector, handler, stopChan)
}

func (s *KubenvoyXDSServer) handleListenerDiscoveryRequest(r *envoy.DiscoveryRequest, stream *XDSStream) {
	glog.V(0).Infof("HandleListenerDiscoveryRequest [%v] %v", r.GetResourceNames(), r.GetResponseNonce())
	handler := s.CreateListenerHandler(r, stream)
	s.listenerConfigWatcher.AddHandler(handler)
}

func parseTargetResourceName(name string) (target *v1.ObjectReference, port int, err error) {
	unsupportedSchemeError := func(name string) error {
		return fmt.Errorf("unsupported scheme name %v, the format must be kubenvoy-managed.srv.namespace:port", name)
	}

	if !strings.HasPrefix(name, kubenvoyTargetPrefix) {
		err = unsupportedSchemeError(name)
		return
	}
	name = strings.TrimPrefix(name, kubenvoyTargetPrefix)

	strs := strings.Split(name, ":")
	if len(strs) != 2 {
		err = unsupportedSchemeError(name)
		return
	}

	host, portStr := strs[0], strs[1]
	strs = strings.Split(host, ".")
	if len(strs) != 2 {
		err = unsupportedSchemeError(name)
		return
	}
	port, err = strconv.Atoi(portStr)
	if err != nil {
		err = unsupportedSchemeError(name)
		return
	}

	return &v1.ObjectReference{
		Name:      strs[0],
		Namespace: strs[1],
	}, port, nil
}
