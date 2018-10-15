package kubenvoyxds

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"

	"kubenvoyxds/utils"

	envoy "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	"github.com/golang/glog"
)

// XDSStream implements (and also wraps) EndpointDiscoveryService_StreamEndpointsServer.
// It has customized Recv() and Send() functions, aiming for simplying streaming of
// DiscoveryRequest & DiscoveryResponse.
type XDSStream struct {
	grpc.ServerStream
	responseChan chan *envoy.DiscoveryResponse

	ackChan map[string]chan *envoy.DiscoveryRequest
	mutex   sync.Mutex

	ctx context.Context

	// TypeURL/name to Version map
	appliedVersion map[string]string
	versionMutex   sync.RWMutex
}

// NewXDSStream wraps EndpointDiscoveryService_StreamEndpointsServer
func NewXDSStream(stream grpc.ServerStream) *XDSStream {
	ctx, cancel := context.WithCancel(stream.Context())
	utils.OnTerminate(cancel)

	return &XDSStream{
		ServerStream: stream,
		ctx:          ctx,
		responseChan: make(chan *envoy.DiscoveryResponse),
		ackChan:      make(map[string]chan *envoy.DiscoveryRequest),

		appliedVersion: make(map[string]string),
	}
}

func (s *XDSStream) Context() context.Context {
	return s.ctx
}

// listen listens responses sent to responseChan and then send them into stream.
// GRPC doesn't allow multiple go routines calling one
// EndpointDiscoveryService_StreamEndpointsServer.Send so we created a channel instead
func (s *XDSStream) listen() {
	for {
		select {
		case resp, ok := <-s.responseChan:
			if !ok {
				return
			}
			if err := s.ServerStream.SendMsg(resp); err != nil {
				glog.Error(err)
				return
			}
		case <-s.Context().Done():
			return
		}
	}
}

// Recv implements one method of EndpointDiscoveryService_StreamEndpointsServer
func (s *XDSStream) Recv() (*envoy.DiscoveryRequest, error) {
	r := new(envoy.DiscoveryRequest)
	if err := s.ServerStream.RecvMsg(r); err != nil {
		return nil, err
	}

	glog.V(1).Infof("Received request %v", r)

	if r.GetResponseNonce() == "" {
		// first request from envoy, likely when envoy server starts
		return r, nil
	}

	s.updateAppliedVersion(r.GetTypeUrl(), strings.Join(r.GetResourceNames(), "|"), r.VersionInfo)

	ackChan, exist := s.ackChanForResponse(r.GetResponseNonce())
	if !exist {
		// nonce not found in this server, we will then process it as if it's new
		return r, nil
	}

	// At this point, nonce is found in this server, so the request is a ACK/NACK for
	// our response. Here we then send the request to ackChan and let original go routine
	// handle it. And since the request is merely a ACK/NACK, we skip it and get next
	// request
	go func() {
		ackChan <- r
		close(ackChan)
	}()

	return s.Recv()
}

func (s *XDSStream) ackChanForResponse(nonce string) (ackChan chan *envoy.DiscoveryRequest, exist bool) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	ackChan, exist = s.ackChan[nonce]
	return
}

// ACK channel for response, identified by nonce, nonce must be unique
func (s *XDSStream) getOrCreateAckChanForResponse(nonce string) chan *envoy.DiscoveryRequest {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	_, exist := s.ackChan[nonce]
	if !exist {
		s.ackChan[nonce] = make(chan *envoy.DiscoveryRequest)
	}
	return s.ackChan[nonce]
}

func (s *XDSStream) deleteAckChanForRespoonse(nonce string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	delete(s.ackChan, nonce)
}

func (s *XDSStream) send(resp *envoy.DiscoveryResponse, count int) error {
	const MaximumRetrial = 10
	if count == MaximumRetrial {
		return fmt.Errorf("failed to receive ACK/NAK from client for response %v, maximum retrials reached", resp)
	}
	s.responseChan <- resp

	ack := s.getOrCreateAckChanForResponse(resp.GetNonce())
	timer := time.NewTimer(20 * time.Second)
	for {
		select {
		case <-s.Context().Done():
			return nil
		case <-timer.C:
			glog.Warningf("failed to receive ACK/NAK from client for DiscoveryResponse %v, retry", resp.Nonce)
			return s.send(resp, count+1)
		case r := <-ack:
			s.deleteAckChanForRespoonse(r.GetResponseNonce())
			if r.GetTypeUrl() != resp.GetTypeUrl() {
				return fmt.Errorf("got unexpected type in ACK request %v", r)
			}
			if r.GetVersionInfo() != resp.GetVersionInfo() {
				return fmt.Errorf("client %v NACKed new config %v: %v", r.GetNode().GetId(), resp.GetVersionInfo(), r.GetErrorDetail())
			}
			s.updateAppliedVersion(r.GetTypeUrl(), strings.Join(r.GetResourceNames(), "|"), r.VersionInfo)
			glog.V(0).Infof("Client %v ACKed new config %v for %v [type: %v]", r.GetNode().GetId(), resp.GetVersionInfo(), r.GetResourceNames(), r.GetTypeUrl())
			return nil
		}
	}
}

func (s *XDSStream) updateAppliedVersion(typeURL, resourceNames, appliedVersion string) {
	s.versionMutex.Lock()
	defer s.versionMutex.Unlock()
	s.appliedVersion[typeURL+"/"+resourceNames] = appliedVersion
}

func (s *XDSStream) AppliedVersion(typeURL, resourceNames string) string {
	s.versionMutex.RLock()
	defer s.versionMutex.RUnlock()
	return s.appliedVersion[typeURL+"/"+resourceNames]
}

// Send implements Send in EndpointDiscoveryService_StreamEndpointsServer interface and overrides the original one.
// s.listen() must be called before any Send() calls
func (s *XDSStream) Send(resp *envoy.DiscoveryResponse) error {
	glog.V(2).Infof("Sending response %v", resp)
	go func() {
		if err := s.send(resp, 0); err != nil {
			glog.Errorf("failed to send new config to client %v", err)
		}
	}()
	return nil
}
