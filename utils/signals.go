package utils

import (
	"os"
	"os/signal"
	"syscall"
)

// OnTerminate triggers input function when OS signals termination & interrupt
func OnTerminate(f func()) {
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		f()
	}()
}

// StopChanOnTerminate creates a stop channel that closes on termination.
func StopChanOnTerminate() chan struct{} {
	c := make(chan struct{})
	OnTerminate(func() {
		close(c)
	})
	return c
}
