package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	sbxbackend "e2b-local/internal/backends/sbx"
)

func main() {
	var relayAddress string
	var token string
	var targetPort int
	var connections int
	flag.StringVar(&relayAddress, "relay", "", "host:port of the e2b-local relay")
	flag.StringVar(&token, "token", "", "per-sandbox relay token")
	flag.IntVar(&targetPort, "target-port", 0, "guest loopback port to forward")
	flag.IntVar(&connections, "connections", 1, "number of reverse connections to keep ready")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := sbxbackend.RunTunnel(ctx, sbxbackend.TunnelSpec{
		RelayAddress: relayAddress,
		Token:        token,
		TargetPort:   targetPort,
		Connections:  connections,
		OnError: func(err error) {
			fmt.Fprintf(os.Stderr, "sbx-tunnel relay=%s target_port=%d: %v\n", relayAddress, targetPort, err)
		},
	}); err != nil && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "sbx-tunnel: %v\n", err)
		os.Exit(1)
	}
}
