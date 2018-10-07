package kubenvoyxds

import (
	"context"
	"fmt"
	"hash"
	"io"
	"strconv"
	"strings"
	"sync"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"log"

	envoy "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	envoyCore "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	envoyEndpoint "github.com/envoyproxy/go-control-plane/envoy/api/v2/endpoint"
	"github.com/gogo/protobuf/proto"
	"github.com/gogo/protobuf/types"
	"github.com/google/uuid"
	"github.com/minio/highwayhash"
)

var hhash hash.Hash64

type KubenvoyXDSServer struct {
	k8sClientSet *kubernetes.Clientset
	watcher      *K8ServiceEndpointsWatcher
}

type EDSTarget struct {
	namespace string
	service   string
	port      int
}

func (t *EDSTarget) String() string {
	return fmt.Sprintf("%v.%v:%v", t.service, t.namespace, t.port)
}

func NewKubenvoyXDSServer(masterURL string, kubeConfigPath string) *KubenvoyXDSServer {
	// creates the connection
	config, err := clientcmd.BuildConfigFromFlags(masterURL, kubeConfigPath)
	if err != nil {
		log.Fatal(err)
	}

	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal(err)
	}

	server := &KubenvoyXDSServer{
		k8sClientSet: clientset,
		watcher:      NewK8ServiceEndpointsWatcher(clientset),
	}

	// respC := make(chan *envoy.DiscoveryResponse)
	// go func() {
	// 	for {
	// 		log.Print("Sending %v", <-respC)
	// 	}
	// }()
	// target, err := parseTargetResourceName("checkinserver.default:8090")
	// if err != nil {
	// 	log.Print("failed to parse target.")
	// } else {
	// 	go server.watcher.WatchService(context.Background(), target, server.CreateEndpointsEventHandler(respC))
	// }

	// go func() {
	// 	time.Sleep(5 * time.Second)
	// 	respC2 := make(chan *envoy.DiscoveryResponse)
	// 	go func() {
	// 		for {
	// 			log.Print("Sending222 %v", <-respC2)
	// 		}
	// 	}()
	// 	target, err = parseTargetResourceName("checkinserver.default:8090")
	// 	if err != nil {
	// 		log.Print("failed to parse target.")
	// 	} else {
	// 		server.watcher.WatchService(context.Background(), target, server.CreateEndpointsEventHandler(respC2))
	// 	}
	// }()

	return server
}

type K8ServiceEndpointsWatcher struct {
	k8sClientSet *kubernetes.Clientset
	watching     map[string]context.CancelFunc
	mutex        *sync.RWMutex
}

func NewK8ServiceEndpointsWatcher(clientset *kubernetes.Clientset) *K8ServiceEndpointsWatcher {
	return &K8ServiceEndpointsWatcher{
		k8sClientSet: clientset,
		watching:     make(map[string]context.CancelFunc),
		mutex:        &sync.RWMutex{},
	}
}

// EndpointsEventHandler handles kubernetes event with a target context, those events are
// usually from results of watch
type EndpointsEventHandler func(target *EDSTarget, event *watch.Event)

// WatchService watches specified target and then process the events with handler. It blocks until
// context is cancelled.
func (w *K8ServiceEndpointsWatcher) WatchService(ctx context.Context, target *EDSTarget, handler EndpointsEventHandler) {
	w.mutex.Lock()
	cancel, exist := w.watching[target.String()]
	if exist {
		// cancel existing one, start a new one
		cancel()
	}
	ctx, w.watching[target.String()] = context.WithCancel(ctx)
	w.mutex.Unlock()

	watchEndpoints(w.k8sClientSet, ctx, target, handler)
}

func (s *KubenvoyXDSServer) CreateEndpointsEventHandler(respChan chan *envoy.DiscoveryResponse) EndpointsEventHandler {
	f := func(target *EDSTarget, event *watch.Event) {
		log.Printf("got event %v", event)

		endpoints, ok := event.Object.(*v1.Endpoints)
		if !ok {
			log.Printf("Unexpected event obj type %v: ", event.Type)
			return
		}

		resp, err := s.generateEDSResponse(target, endpoints)
		if err != nil {
			log.Printf("Failed to generate EDS response: %v", err)
			return
		}

		// Note GRPC doesn't allow multiple go routines calling one stream.Send so
		// we created a channel instead
		respChan <- resp
	}

	return f
}

// StreamEndpoints implements one method of GRPC Envoy XDS service.
func (s *KubenvoyXDSServer) StreamEndpoints(stream envoy.EndpointDiscoveryService_StreamEndpointsServer) error {
	if err := s.listenRequests(stream); err != nil {
		log.Printf("error handling request stream %v", err)
		return err
	}

	return nil
}

func (s *KubenvoyXDSServer) listenRequests(stream envoy.EndpointDiscoveryService_StreamEndpointsServer) error {
	for {
		req, err := stream.Recv()
		log.Printf("Received request %v", req)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		if err := s.handleDiscoveryRequest(req, stream); err != nil {
			return err
		}
	}
}

func (s *KubenvoyXDSServer) generateEDSResponse(target *EDSTarget, endpoints *v1.Endpoints) (*envoy.DiscoveryResponse, error) {
	assignment, err := clusterLoadAssignmentFromEndpoint(target, endpoints)
	if err != nil {
		return nil, fmt.Errorf("failed to get cluster assignment for %v, skip", endpoints)
	}

	any, err := types.MarshalAny(assignment)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal cluster load assignment for %v, skip", assignment)
	}

	r := &envoy.DiscoveryResponse{
		// How to pass this down ??
		TypeUrl:   "type.googleapis.com/envoy.api.envoy.ClusterLoadAssignment",
		Resources: []types.Any{*any},
	}

	fp, err := ProtoFingerprint(r)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal discovery response %v. this should not happen", r)
	}
	r.VersionInfo = strconv.FormatUint(fp, 36)
	r.Nonce = uuid.New().String()

	return r, nil
}

func streamResponse(ctx context.Context, respChan chan *envoy.DiscoveryResponse, stream envoy.EndpointDiscoveryService_StreamEndpointsServer) {
	// stream response
	// this is mainly for concurency safety. grpc.Send must be called on same
	// go routine.
	for {
		select {
		case resp := <-respChan:
			log.Printf("Sending response %v", resp)
			err := stream.Send(resp)
			if err != nil {
				log.Printf("Error sending discovery response to client: %v", err)
			}
		case <-ctx.Done():
			return
		}
	}
}

func (s *KubenvoyXDSServer) handleDiscoveryRequest(r *envoy.DiscoveryRequest, stream envoy.EndpointDiscoveryService_StreamEndpointsServer) error {
	log.Printf("handleDiscoveryRequest %v", r.GetResourceNames())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	respChan := make(chan *envoy.DiscoveryResponse)
	go streamResponse(ctx, respChan, stream)

	wg := sync.WaitGroup{}
	errors := []error{}
	for _, r := range r.GetResourceNames() {
		target, err := parseTargetResourceName(r)
		if err != nil {
			errors = append(errors, fmt.Errorf("Failed to parse resource %v: %v. Skipping it", r, err))
			continue
		}

		handler := s.CreateEndpointsEventHandler(respChan)

		wg.Add(1)
		go func() {
			s.watcher.WatchService(context.Background(), target, handler)
			wg.Done()
		}()
	}

	wg.Wait()
	log.Printf("handleDiscoveryRequest Done !!! %v", r.GetResourceNames())
	if len(errors) != 0 {
		return fmt.Errorf("Errors %v", errors)
	}
	return nil
}

// FetchEndpoints not implemented
func (s *KubenvoyXDSServer) FetchEndpoints(ctx context.Context, r *envoy.DiscoveryRequest) (*envoy.DiscoveryResponse, error) {
	log.Printf("Got FetchEndpoints requests %v", r)
	return nil, grpc.Errorf(codes.Unimplemented, "")
}

func clusterLoadAssignmentFromEndpoint(target *EDSTarget, endpoints *v1.Endpoints) (*envoy.ClusterLoadAssignment, error) {
	type Address = envoyCore.Address
	type SocketAddress = envoyCore.SocketAddress
	type Address_SocketAddress = envoyCore.Address_SocketAddress
	type SocketAddress_PortValue = envoyCore.SocketAddress_PortValue

	lbendpoints := []envoyEndpoint.LbEndpoint{}
	for _, subset := range endpoints.Subsets {
		port := uint32(target.port)
		for _, address := range subset.Addresses {
			lbendpoints = append(lbendpoints, envoyEndpoint.LbEndpoint{
				Endpoint: &envoyEndpoint.Endpoint{
					Address: &Address{Address: &Address_SocketAddress{&SocketAddress{
						Address:       address.IP,
						PortSpecifier: &SocketAddress_PortValue{port},
					},
					}},
				},
			})
		}
	}

	assignment := envoy.ClusterLoadAssignment{
		Endpoints: []envoyEndpoint.LocalityLbEndpoints{
			envoyEndpoint.LocalityLbEndpoints{
				LbEndpoints: lbendpoints,
			},
		},
	}

	return &assignment, nil
}

func parseTargetResourceName(name string) (*EDSTarget, error) {
	unsupportedSchemeError := func(name string) error {
		return fmt.Errorf("unsupported scheme name %v, the format must be srv.namespace:port", name)
	}

	strs := strings.Split(name, ":")
	if len(strs) != 2 {
		return nil, unsupportedSchemeError(name)
	}

	host, portStr := strs[0], strs[1]
	strs = strings.Split(host, ".")
	if len(strs) != 2 {
		return nil, unsupportedSchemeError(name)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, unsupportedSchemeError(name)
	}

	return &EDSTarget{
		service:   strs[0],
		namespace: strs[1],
		port:      port,
	}, nil
}

func ProtoFingerprint(msg proto.Message) (uint64, error) {
	key := []byte("my hobby bloa as a brother ok ?)")
	data, err := proto.Marshal(msg)
	if err != nil {
		return 0, err
	}

	hhash, _ = highwayhash.New64(key)
	hhash.Reset()
	hhash.Write(data)
	return hhash.Sum64(), nil
}
