package kubenvoyxds

import (
	"context"
	"flag"
	"log"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"

	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
)

var watchConnectionMaxTime = flag.Int64("watch_max_keep_alive", 60, "Maximum time to keep connection alive on watch")

func watchEndpoints(clientset *kubernetes.Clientset, ctx context.Context, target *EDSTarget, handler EventHandler) {
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
			log.Printf("connection (re)start for %v", target)
			endpointsWatch, err := clientset.CoreV1().Endpoints(target.namespace).Watch(metav1.ListOptions{
				FieldSelector: endpointsSelector, TimeoutSeconds: watchConnectionMaxTime})
			if err != nil {
				log.Print(err)
				time.Sleep(10 * time.Second)
				go func() {
					next <- true
				}()
				log.Printf("break for %v", target)
				break
			}

			go func(watchInterface watch.Interface) {
				watchResultsWithTicker(endpointsWatch.ResultChan(), target, handler, 60*time.Second)
				endpointsWatch.Stop()
				next <- true
			}(endpointsWatch)
		}
	}
}

func watchResultsWithTicker(resultChan <-chan watch.Event, target *EDSTarget, handler EventHandler, duration time.Duration) {
	ticker := time.NewTicker(duration)
	start := time.Now()
	for {
		select {
		case <-ticker.C:
			log.Printf("Not able to get any results for %v after %v", target, time.Since(start))
		case event, ok := <-resultChan:
			ticker.Stop()
			if !ok {
				log.Print("Watch channel closed")
				return
			}
			handler(target, &event)
		}
	}
}
