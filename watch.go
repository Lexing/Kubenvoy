package kubenvoyxds

import (
	"context"
	"flag"
	"log"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
)

var watchConnectionMaxTime = flag.Int64("watch_max_keep_alive", 60, "Maximum time to keep connection alive on watch")

// func watchAndHandleEvents() {
// 	for {
// 		event, ok := <-watch.ResultChan()
// 		if !ok {
// 			log.Print("done close")
// 			watch.Stop()
// 			next <- true
// 			break
// 		}
// 		log.Print(event)
// 	}
// }

func watchEndpoints(clientset *kubernetes.Clientset, ctx context.Context, target *EDSTarget, handler EndpointsEventHandler) {

	next := make(chan bool)
	go func() {
		next <- true
	}()
	log.Printf("watchEndpoints started %v", target.String())

	endpointsSelector := fields.Set{"metadata.name": target.service}.AsSelector().String()

	for {
		select {
		case <-ctx.Done():
			log.Printf("watchEndpoints cancelled %v", target.String())
			return
		case <-next:
			log.Print("start")
			watch, err := clientset.CoreV1().Endpoints(target.namespace).Watch(metav1.ListOptions{
				FieldSelector: endpointsSelector, TimeoutSeconds: watchConnectionMaxTime})
			if err != nil {
				log.Print(err)
				time.Sleep(10 * time.Second)
				go func() {
					next <- true
				}()
				log.Print("break")
				break
			}
			log.Print("error pass")

			go func() {
				for {
					event, ok := <-watch.ResultChan()
					if !ok {
						log.Print("done close")
						watch.Stop()
						next <- true
						break
					}
					handler(target, &event)
				}
			}()
		}
	}
}
