//go:build windows

package main

import (
	"log/slog"
	"os"
	"os/signal"

	"github.com/ben-kuhn/tncd/v2/internal/app"
	"golang.org/x/sys/windows/svc"
)

// isWindowsService reports whether the process was started by the Service
// Control Manager (vs. an interactive console).
func isWindowsService() bool {
	is, err := svc.IsWindowsService()
	if err != nil {
		return false
	}
	return is
}

// installLogging sets the default slog logger. The service event-log path is
// added in Task 2; for now Windows always logs to stderr.
func installLogging(level slog.Level, _ bool) {
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(h))
}

// run blocks until the Runtime shuts down. The SCM branch is added in Task 2;
// for now Windows always runs interactively (Ctrl+C).
func run(r *app.Runtime, _ bool) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		slog.Info("Received signal, shutting down...")
		r.Shutdown()
	}()
	r.Wait()
}
