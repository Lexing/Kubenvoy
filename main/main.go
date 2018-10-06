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

var (
	address string
)

func init() {
	flag.StringVar(&address, "address", ":8090", "Address to listen on")
}

func main() {
	flag.Parse()

	rpcs := grpc.NewServer()
	v2.RegisterEndpointDiscoveryServiceServer(rpcs, kubenvoyxds.NewKubenvoyXDSServer())
	reflection.Register(rpcs)

	lis, err := net.Listen("tcp", address)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	log.Printf("GRPC server listens on address %v\n", address)

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		rpcs.GracefulStop()
	}()

	if err := rpcs.Serve(lis); err != nil {
		log.Fatalf("failed to start rpc server: %v", err)
	}
}
