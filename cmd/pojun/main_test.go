package main

import (
	"context"
	"testing"

	"github.com/chu0119/pojun/agent"
)

func TestDefaultListenAddrIsLoopback(t *testing.T) {
	if defaultListenAddr != "127.0.0.1:8787" {
		t.Fatalf("defaultListenAddr = %q, want 127.0.0.1:8787", defaultListenAddr)
	}
	if !isLoopbackListenAddr(defaultListenAddr) {
		t.Fatalf("isLoopbackListenAddr(%q) = false, want true", defaultListenAddr)
	}
}

func TestIsLoopbackListenAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8787", true},
		{"localhost:8787", true},
		{"[::1]:8787", true},
		{":8787", false},
		{"0.0.0.0:8787", false},
		{"[::]:8787", false},
		{"192.168.1.5:8787", false},
		{"bad", false},
	}
	for _, c := range cases {
		if got := isLoopbackListenAddr(c.addr); got != c.want {
			t.Errorf("isLoopbackListenAddr(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

func TestShutdownContextPreservesNamedCause(t *testing.T) {
	signalCtx, signalCancel := context.WithCancel(context.Background())
	ctx, shutdown := shutdownContext(signalCtx)
	defer shutdown(nil)
	signalCancel()
	<-ctx.Done()
	if code, _, _, ok := agent.AbortReason(ctx); !ok || code != "shutdown" {
		t.Fatalf("code=%q ok=%v, want shutdown", code, ok)
	}
}
