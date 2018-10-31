package kubenvoy

import (
	"flag"
	"fmt"
	"strings"

	envoy "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	envoyCore "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	envoyBootstrap "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v2"
	"github.com/ghodss/yaml"
	"github.com/gogo/protobuf/jsonpb"
	"github.com/gogo/protobuf/proto"
	"github.com/golang/glog"
)

var (
	edsClusterName = flag.String("eds_cluster_name", "xds_cluster", "XDS cluster name.")
)

const defaultClusterConfig = `connect_timeout: 1.00s
http2_protocol_options: {}
type: EDS
lb_policy: ROUND_ROBIN
`

var defaultCluster *envoy.Cluster

func yamlToEnvoyListener(data []byte) ([]envoy.Listener, error) {
	resources := envoyBootstrap.Bootstrap_StaticResources{}
	err := yamlToProto(data, &resources)
	if err != nil {
		return nil, err
	}
	return resources.GetListeners(), nil
}

func yamlToEnvoyCluster(data []byte) (*envoy.Cluster, error) {
	cluster := envoy.Cluster{}
	err := yamlToProto(data, &cluster)
	if err != nil {
		return nil, err
	}
	return &cluster, nil
}

func yamlToProto(data []byte, pb proto.Message) error {
	data, err := yaml.YAMLToJSON(data)
	if err != nil {
		return fmt.Errorf("cannot parse YAML file to JSON file: %v", err)
	}
	if err := jsonpb.Unmarshal(strings.NewReader(string(data)), pb); err != nil {
		return fmt.Errorf("failed to unmarshal json to %t: %v", pb, err)
	}
	return nil
}

var xdsConfigSource *envoyCore.ConfigSource

func init() {
	xdsConfigSource = &envoyCore.ConfigSource{
		ConfigSourceSpecifier: &envoyCore.ConfigSource_ApiConfigSource{ApiConfigSource: &envoyCore.ApiConfigSource{
			ApiType: envoyCore.ApiConfigSource_GRPC,
			GrpcServices: []*envoyCore.GrpcService{
				&envoyCore.GrpcService{TargetSpecifier: &envoyCore.GrpcService_EnvoyGrpc_{
					EnvoyGrpc: &envoyCore.GrpcService_EnvoyGrpc{
						ClusterName: *edsClusterName,
					},
				}},
			},
		}},
	}

	var err error
	defaultCluster, err = yamlToEnvoyCluster([]byte(defaultClusterConfig))
	if err != nil {
		glog.Fatalf("fail to initialize: %v", err)
	}
	defaultCluster.EdsClusterConfig = &envoy.Cluster_EdsClusterConfig{
		EdsConfig: xdsConfigSource,
	}
}
