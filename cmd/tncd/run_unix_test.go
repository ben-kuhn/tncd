//go:build !windows

package main

import (
	"syscall"
	"testing"
	"time"

	"github.com/ben-kuhn/tncd/v2/internal/app"
	"github.com/ben-kuhn/tncd/v2/internal/config"
)

// TestRunUnixShutsDownOnSIGTERM verifies run() installs a SIGINT/SIGTERM
// handler that drives Runtime.Shutdown, so run() returns after a SIGTERM.
func TestRunUnixShutsDownOnSIGTERM(t *testing.T) {
	cfg := &config.Config{
		Server: config.Server{ListenHost: "127.0.0.1", ListenPort: 0, Callsign: "TEST", MaxClients: 8},
		AX25:   config.AX25{MaxWindow: 3, N2Retry: 10, T3Timeout: 180},
		Ports: []config.Port{{
			Name: "Port 0", Type: "tcp", Host: "127.0.0.1", TCPPort: 1,
			OTABaudrate: 1200, AX25Version: 22, Reconnect: false,
		}},
	}
	r, err := app.New(cfg, 0, 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Install a readiness barrier: signalReady fires right after run() calls
	// signal.Notify, guaranteeing the handler is registered before we send a
	// signal (otherwise SIGTERM would hit the default disposition and kill the
	// test process). Restore the no-op default when done.
	ready := make(chan struct{})
	origReady := signalReady
	signalReady = func() { close(ready) }
	defer func() { signalReady = origReady }()

	done := make(chan struct{})
	go func() { run(r, false); close(done) }()

	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not install signal handler")
	}

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("kill: %v", err)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("run did not return after SIGTERM")
	}
}
