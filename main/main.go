package main

import (
	"flag"
	"kubenvoy"

	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/golang/glog"
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

	rpcs := kubenvoy.NewGRPCKubenvoyXDSServer(*kubeMasterURL, *kubeConfigPath)
	reflection.Register(rpcs)

	lis, err := net.Listen("tcp", *address)
	if err != nil {
		glog.Fatalf("failed to listen: %v", err)
	}

	glog.Infof("Envoy XDS server listens on address %v\n", *address)

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		glog.V(0).Infof("Stopping RPC server.")
		rpcs.Stop()
		glog.V(0).Infof("RPC server stopped")
	}()

	if err := rpcs.Serve(lis); err != nil {
		glog.Fatalf("failed to start rpc server: %v", err)
	}
}
