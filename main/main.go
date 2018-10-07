package main

import (
	"flag"
	"kubenvoyxds"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/envoyproxy/go-control-plane/envoy/api/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

// flags
var (
	address        = flag.String("address", ":8090", "Address to listen on")
	kubeConfigPath = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	kubeMasterURL  = flag.String("kubemaster", "", "master url")
)

func main() {
	flag.Parse()

	server := kubenvoyxds.NewKubenvoyXDSServer(*kubeMasterURL, *kubeConfigPath)

	rpcs := grpc.NewServer()
	v2.RegisterEndpointDiscoveryServiceServer(rpcs, server)
	reflection.Register(rpcs)

	lis, err := net.Listen("tcp", *address)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	log.Printf("Envoy XDS server listens on address %v\n", *address)

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		rpcs.GracefulStop()
		log.Print("Trying to gracefully stop server.")
	}()

	if err := rpcs.Serve(lis); err != nil {
		log.Fatalf("failed to start rpc server: %v", err)
	}
}
