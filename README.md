# Kubenvoy
## Envoy with Kubernetes Discovery & config auto reloading


Kubenvoy is an envoy distribution bundled with XDS for kubernetes, it's designed to work (mostly) as edge or service proxy with built-in discovery. Some good applications are API-gateways & load balancers. 

It was built simply becaucuse [Ambassador](https://www.getambassador.io/) is not good / flexible enough, serveral benefits of Kubenvoy are 

1. Kubenvoy has true load balancing, it provides envoy service endpoints rather than DNS based cluster endpoints. Ambassador doesn't really resolve endpoints so traffics are not balanced across pods for same service (if you don't want to use Headless service in Kubernetes)

2. Kubenvoy's implementation is lighter and more intuitive, it's built with native Envoy V2 API (grpc version)

3. Configuration for Kubenvoy is much more flexible since native envoy config was adopted so we can resuse native config as much as possible, especially for listeners and routes configs.

However, Kubenvoy is also more than an API-gateway, people can use it as in-cluster load balancers as well, e.g. for GRPC.


## To start use Kubenvoy

1. Start Kubenvoy in your Kubernetes cluster. Examples can be seen below, two deployments will be brought up: kubenvoy-envoy & kubenvoy-xds
```
kubectl apply -f example-config/service.yaml
```

2. To make your Kubernetes service discoverable by Kubenvoy, apply label `kubenvoy-discovery=true` to the service with `kubectl label` or `kubectl apply`
```
kubectl label svc [some service] kubenvoy-discovery=true
```


3. Setup or change your listeners/routes in ConfigMap `kubenvoy-xds-config`, using native envoy configs formats. Configs will be automatically updated for envoy. Following is an example of one listener config, specs are [here](https://www.envoyproxy.io/docs/envoy/latest/api-v2/api/v2/lds.proto#envoy-api-msg-listener)
```
apiVersion: v1
kind: ConfigMap
metadata:
  name: kubenvoy-xds-config
data:
  listeners.yaml: |
      - name: listener_1
        address:
          socket_address:
            protocol: TCP
            address: 0.0.0.0
            port_value: 80
        filter_chains:
          - filters:
              - name: envoy.http_connection_manager
                config:
                  codec_type: auto
                  stat_prefix: ingress_http
                  route_config:
                    name: route
                    virtual_hosts:
                      - name: backend
                        domains:
                          - "*"
                        routes:
                          - match:
                              prefix: "/bcd/"
                            route:
                              weighted_clusters:
                                clusters:
                                  - name: kubenvoy://some-svc.namespace:80
                                    weight: 100
                          - match:
                              prefix: "/abc/"
                            route:
                              weighted_clusters:
                                clusters:
                                  - name: kubenvoy://some-other-svc.namespace:80
                                    weight: 100
                  http_filters:
                    - name: envoy.router
                      config: {}
```

Cluster names must be in format `kubenvoy://servicename.namespace:port` for Kubenvoy to map routes correctly. 

