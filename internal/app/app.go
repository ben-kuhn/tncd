// Package app wires the tncd engine, bridge, and frontends into a single
// runnable unit shared by every launch mode (console today; the Windows
// service in a later plan). It owns startup and the graceful shutdown
// sequence so there is exactly one source of truth for both.
package app

import (
	"fmt"
	"log/slog"
	"net"

	"github.com/ben-kuhn/tncd/v2/internal/bridge"
	"github.com/ben-kuhn/tncd/v2/internal/config"
	"github.com/ben-kuhn/tncd/v2/internal/engine"
	agwpeserver "github.com/ben-kuhn/tncd/v2/internal/frontend/agwpe"
	apiserver "github.com/ben-kuhn/tncd/v2/internal/frontend/api"
	kisstcpserver "github.com/ben-kuhn/tncd/v2/internal/frontend/kisstcp"
)

// Runtime is a wired-up tncd instance: engine, bridge, and all enabled
// frontends. Build one with New, then call Wait to run it; Shutdown stops it.
type Runtime struct {
	eng     *engine.Engine
	bridge  *bridge.Bridge
	agwpeLn net.Listener
	kissSrv *kisstcpserver.Server
	apiSrv  *apiserver.Server
}

// New builds the engine and bridge, starts the AGWPE server, and starts the
// KISS-over-TCP and read-only API servers when enabled in cfg. It does not
// block; call Wait to run the engine loop. verbose and traffic set AX.25 frame
// and hex-dump verbosity (0 = off). On any startup error, already-opened
// listeners are closed before returning.
func New(cfg *config.Config, verbose, traffic int) (*Runtime, error) {
	eng := engine.New()
	b := bridge.New(eng, cfg)
	b.SetVerbosity(verbose, traffic)

	if err := b.Start(); err != nil {
		return nil, fmt.Errorf("bridge start: %w", err)
	}
	b.RegisterMonitorSink(agwpeserver.NewMonitorSink(b))

	ln, err := agwpeserver.Serve(eng, b, cfg.Server.ListenHost, cfg.Server.ListenPort)
	if err != nil {
		return nil, fmt.Errorf("agwpe server: %w", err)
	}

	r := &Runtime{eng: eng, bridge: b, agwpeLn: ln}

	if cfg.KISSTCP.Enabled {
		r.kissSrv, err = kisstcpserver.Serve(eng, b, cfg.KISSTCP.ListenHost, cfg.KISSTCP.ListenPort, cfg.KISSTCP.MaxClients)
		if err != nil {
			ln.Close()
			return nil, fmt.Errorf("kisstcp server: %w", err)
		}
		slog.Info("KISS-over-TCP passthrough started",
			"listen", fmt.Sprintf("%s:%d", cfg.KISSTCP.ListenHost, cfg.KISSTCP.ListenPort))
	}

	if cfg.API.Enabled {
		r.apiSrv, err = apiserver.Serve(eng, b, cfg.API.ListenHost, cfg.API.ListenPort, cfg.API.MaxClients, cfg.API.ServeUI)
		if err != nil {
			if r.kissSrv != nil {
				r.kissSrv.Close()
			}
			ln.Close()
			return nil, fmt.Errorf("api server: %w", err)
		}
		slog.Info("read-only API started",
			"listen", fmt.Sprintf("%s:%d", cfg.API.ListenHost, cfg.API.ListenPort))
	}

	return r, nil
}

// AGWPEAddr returns the address the AGWPE server is listening on. Useful when
// the configured port is 0 (ephemeral) and for status/manage displays.
func (r *Runtime) AGWPEAddr() net.Addr { return r.agwpeLn.Addr() }

// Wait runs the engine loop on the calling goroutine, blocking until Shutdown
// completes its teardown (which stops the loop).
func (r *Runtime) Wait() { r.eng.Run() }

// Shutdown posts the graceful teardown sequence to the engine loop. Ordering
// mirrors the original main.go path:
//  1. close AGWPE client transports (so the listener's Accept unblocks),
//  2. close the listeners (AGWPE, KISS-over-TCP, API),
//  3. bridge.Shutdown() (KISS exit strings + port close),
//  4. engine.Stop().
// Safe to call from any goroutine.
func (r *Runtime) Shutdown() {
	r.eng.Do(func() {
		for _, c := range r.bridge.Clients() {
			c.CloseTransport()
		}
		r.agwpeLn.Close()
		if r.kissSrv != nil {
			r.kissSrv.Close()
		}
		if r.apiSrv != nil {
			r.apiSrv.Close()
		}
		r.bridge.Shutdown()
		r.eng.Stop()
	})
}
