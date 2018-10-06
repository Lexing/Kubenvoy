package kubenvoyxds

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"log"

	"github.com/envoyproxy/go-control-plane/envoy/api/v2"
	"github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	"github.com/envoyproxy/go-control-plane/envoy/api/v2/endpoint"
	"github.com/gogo/protobuf/types"
)

type KubenvoyXDSServer struct {
	k8sClient      K8sClient
	stream         v2.EndpointDiscoveryService_StreamEndpointsServer
	serviceWatcher *K8ServiceEndpointsWatcher
	todo           []v2.DiscoveryRequest
}

type EDSTarget struct {
	namespace string
	service   string
	port      int
}

func NewKubenvoyXDSServer() *KubenvoyXDSServer {
	// client, err := NewInClusterK8sClient()
	// if err != nil {
	// 	log.Fatal("fail to create XDS server.")
	// }
	client := NewInsecureK8sClient("http://localhost:8001")
	return &KubenvoyXDSServer{
		k8sClient:      client,
		serviceWatcher: NewK8ServiceEndpointsWatcher(client),
	}
}

type K8ServiceEndpointsWatcher struct {
	k8sClient K8sClient
	watchChan chan *EDSTarget

	mutex sync.RWMutex
}

type EndpointsHandler func(target *EDSTarget, endpoints Endpoints)

func NewK8ServiceEndpointsWatcher(client K8sClient) *K8ServiceEndpointsWatcher {
	return &K8ServiceEndpointsWatcher{
		k8sClient: client,
	}
}

func (w *K8ServiceEndpointsWatcher) WatchService(ctx context.Context, target *EDSTarget, handler EndpointsHandler) error {
	log.Printf("watchEndpoints %v", target)
	sw, err := watchEndpoints(w.k8sClient, target.namespace, target.service)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case up, hasMore := <-sw.ResultChan():
			if hasMore {
				handler(target, up.Object)
			} else {
				return nil
			}
		}
	}
}

func (s *KubenvoyXDSServer) StreamEndpoints(stream v2.EndpointDiscoveryService_StreamEndpointsServer) error {
	s.stream = stream
	if err := s.listenRequests(); err != nil {
		log.Printf("error handling request stream %v", err)
		return err
	}

	return nil
}

func (s *KubenvoyXDSServer) listenRequests() error {
	for {
		req, err := s.stream.Recv()
		log.Printf("Received request %v", req)
		if err == io.EOF {
			log.Print("err == io.EOF")
			return nil
		}
		if err != nil {
			log.Print("err != nil")
			return err
		}

		if err := s.handleDiscoveryRequest(req); err != nil {
			return err
		}
	}
}

func (s *KubenvoyXDSServer) generateEDSResponse(target *EDSTarget, endpoints Endpoints) (*v2.DiscoveryResponse, error) {
	assignment, err := clusterLoadAssignmentFromEndpoint(target, endpoints)
	if err != nil {
		return nil, fmt.Errorf("failed to get cluster assignment for %v, skip", endpoints)
	}

	any, err := types.MarshalAny(assignment)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal cluster load assignment for %v, skip", assignment)
	}

	return &v2.DiscoveryResponse{
		// How to pass this down ??
		TypeUrl:   "type.googleapis.com/envoy.api.v2.ClusterLoadAssignment",
		Resources: []types.Any{*any},
	}, nil
}

func (s *KubenvoyXDSServer) handleEndpoints(target *EDSTarget, endpoints Endpoints) {
	log.Printf("got endpoints %v", endpoints)

	resp, err := s.generateEDSResponse(target, endpoints)
	if err != nil {
		log.Printf("Failed to generate EDS response: %v", err)
	}

	log.Printf("Sending response %v", resp)

	err = s.stream.Send(resp)
	if err != nil {
		log.Printf("Error sending discovery response to client: %v", err)
	}
}

func (s *KubenvoyXDSServer) handleDiscoveryRequest(r *v2.DiscoveryRequest) error {
	log.Printf("handleDiscoveryRequest %v", r.GetResourceNames())

	for _, r := range r.GetResourceNames() {

		target, err := parseTargetResourceName(r)
		if err != nil {
			log.Printf("Failed to get parse resource %v: %v. Skipping it", r, err)
			continue
		}

		// w.mutex.Lock()
		// w.watching
		// w.mutex.Unlock()

		err = s.serviceWatcher.WatchService(context.Background(), target, s.handleEndpoints)
		if err != nil {
			log.Printf("failed to watch service %v: %v", r, err)
		}
	}

	return nil
}

func (s *KubenvoyXDSServer) FetchEndpoints(ctx context.Context, r *v2.DiscoveryRequest) (*v2.DiscoveryResponse, error) {
	return nil, grpc.Errorf(codes.Unimplemented, "")
}

func clusterLoadAssignmentFromEndpoint(target *EDSTarget, endpoints Endpoints) (*v2.ClusterLoadAssignment, error) {
	lbendpoints := []endpoint.LbEndpoint{}
	for _, subset := range endpoints.Subsets {
		port := uint32(target.port)
		for _, address := range subset.Addresses {
			lbendpoints = append(lbendpoints, endpoint.LbEndpoint{
				Endpoint: &endpoint.Endpoint{
					Address: &core.Address{Address: &core.Address_SocketAddress{&core.SocketAddress{
						Address:       address.IP,
						PortSpecifier: &core.SocketAddress_PortValue{port},
					},
					}},
				},
			})
		}
	}

	assignment := v2.ClusterLoadAssignment{
		Endpoints: []endpoint.LocalityLbEndpoints{
			endpoint.LocalityLbEndpoints{
				LbEndpoints: lbendpoints,
			},
		},
	}

	return &assignment, nil
}

func parseTargetResourceName(name string) (target *EDSTarget, err error) {
	strs := strings.Split(name, ":")
	if len(strs) != 2 {
		return nil, fmt.Errorf("unsupported scheme name %v, the format must be srv.namespace:port", name)
	}

	host, portStr := strs[0], strs[1]
	strs = strings.Split(host, ".")
	if len(strs) != 2 {
		return nil, fmt.Errorf("unsupported scheme name %v, the format must be srv.namespace:port", name)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("unsupported scheme name %v, the format must be srv.namespace:port", name)
	}

	return &EDSTarget{
		service:   strs[0],
		namespace: strs[1],
		port:      port,
	}, nil
}
