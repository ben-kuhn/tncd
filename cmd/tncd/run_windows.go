//go:build windows

package main

import (
	"log/slog"
	"os"
	"os/signal"

	"github.com/ben-kuhn/tncd/v2/internal/app"
	"golang.org/x/sys/windows/svc"
)

const serviceName = "tncd"

// isWindowsService reports whether the process was started by the Service
// Control Manager (vs. an interactive console).
func isWindowsService() bool {
	is, err := svc.IsWindowsService()
	if err != nil {
		return false
	}
	return is
}

// installLogging sets the default slog logger. When the process is service-
// hosted there is no console, so logs go to the Windows Event Log; if the Event
// Log source cannot be opened (e.g. not yet registered) it falls back to stderr.
// Interactive Windows runs always log to stderr.
func installLogging(level slog.Level, service bool) {
	if service {
		if h, err := newEventLogHandler(serviceName, level); err == nil {
			slog.SetDefault(slog.New(h))
			return
		}
		// fall through to stderr on failure
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(h))
}

// run blocks until the Runtime shuts down. When service-hosted it hands control
// to the SCM via svc.Run; otherwise it runs interactively (Ctrl+C).
func run(r *app.Runtime, service bool) {
	if service {
		if err := svc.Run(serviceName, &tncdService{r: r}); err != nil {
			slog.Error("service run failed", "err", err)
			r.Shutdown()
		}
		return
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		slog.Info("Received signal, shutting down...")
		r.Shutdown()
	}()
	r.Wait()
}

// tncdService adapts internal/app.Runtime to the SCM's svc.Handler contract.
type tncdService struct{ r *app.Runtime }

// Execute runs the service: report StartPending, run the engine loop in a
// goroutine, report Running, then service Stop/Shutdown by driving the
// Runtime's graceful teardown. Interrogate echoes current status.
func (s *tncdService) Execute(_ []string, req <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}

	done := make(chan struct{})
	go func() { s.r.Wait(); close(done) }()

	changes <- svc.Status{State: svc.Running, Accepts: accepted}
	for {
		select {
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				s.r.Shutdown()
				<-done
				changes <- svc.Status{State: svc.Stopped}
				return false, 0
			default:
				slog.Warn("unexpected service control request", "cmd", c.Cmd)
			}
		case <-done:
			// Engine stopped on its own (e.g. fatal internal error).
			changes <- svc.Status{State: svc.Stopped}
			return false, 0
		}
	}
}
