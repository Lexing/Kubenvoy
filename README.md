# Kubenvoy
## Envoy with Kubernetes Discovery XDS & Config Auto Reload


Kubenvoy is an envoy distribution bundled with XDS for kubernetes, it's designed to work (mostly) as API gateway, similar to Ambassador. 
However, there are many different aspects for Kubenvoy from Ambassador 

1. Kubenvoy's implementation is lighter, it's built with Envoy V2 grpc API 
2. Configuration in Kubenvoy is much more flexible since we can reuse native envoy config as much as possible, especially for listeners and routes configs.


## To start use Kubenvoy

1. Start Kubenvoy in your Kubernetes cluster. Examples can be seen below, two deployments will be brought up: kubenvoy-envoy & kubenvoy-xds
```
kubectl apply -f example-config/service.yaml
```

2. Make your service discoverable by Kubenvoy with following labels, you can also use other methods, e.g `kubectl apply` to add labels too
```
kubectl label svc [some service] kubenvoy-discovery=true
```


3. Set up or change your listeners/routes in ConfigMap `kubenvoy-xds-config`, using native envoy configs formats, something like:
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

