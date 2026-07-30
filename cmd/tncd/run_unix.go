//go:build !windows

package main

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ben-kuhn/tncd/v2/internal/app"
)

// isWindowsService is always false off Windows.
func isWindowsService() bool { return false }

// installLogging sets the default slog logger. Off Windows there is no service
// event log, so logging always goes to stderr. The service argument is ignored.
func installLogging(level slog.Level, _ bool) {
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(h))
}

// signalReady is called immediately after the signal handler is installed. It
// is a no-op in production; tests override it to synchronize before sending a
// signal.
var signalReady = func() {}

// run blocks until the Runtime shuts down. On Unix, SIGINT/SIGTERM triggers the
// graceful 4-step shutdown. The service argument is always false here.
func run(r *app.Runtime, _ bool) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	signalReady()
	go func() {
		sig := <-sigCh
		slog.Info("Received signal, shutting down...", "signal", sig)
		r.Shutdown()
	}()
	r.Wait()
}
