// Package app wires the tncd engine, bridge, and frontends into a single
// runnable unit shared by every launch mode (console today; the Windows
// service in a later plan). It owns startup and the graceful shutdown
// sequence so there is exactly one source of truth for both.
package app

import (
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"

	"github.com/ben-kuhn/tncd/v2/internal/bridge"
	"github.com/ben-kuhn/tncd/v2/internal/config"
	"github.com/ben-kuhn/tncd/v2/internal/engine"
	agwpeserver "github.com/ben-kuhn/tncd/v2/internal/frontend/agwpe"
	apiserver "github.com/ben-kuhn/tncd/v2/internal/frontend/api"
	kisstcpserver "github.com/ben-kuhn/tncd/v2/internal/frontend/kisstcp"
	"github.com/ben-kuhn/tncd/v2/internal/frontend/rigctl"
	"github.com/ben-kuhn/tncd/v2/internal/netutil"
	"github.com/ben-kuhn/tncd/v2/internal/rig"
)

// Runtime is a wired-up tncd instance: engine, bridge, and all enabled
// frontends. Build one with New, then call Wait to run it; Shutdown stops it.
type Runtime struct {
	eng     *engine.Engine
	bridge  *bridge.Bridge
	agwpeLn net.Listener
	kissSrv *kisstcpserver.Server
	apiSrv  *apiserver.Server
	// rigSrvs holds one rigctl.Server per [rigctl.N] section with Enabled =
	// true -- zero, one, or several, matching cfg.RigCtl by construction
	// order in New. Nil/empty when rig control is not configured anywhere.
	rigSrvs []*rigctl.Server
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
	b.RegisterRawRXSink(agwpeserver.NewRawSink(b))

	warnIfExposed("agwpe", cfg.Server.ListenHost, cfg.Server.ListenPort, cfg.Server.AllowedSubnets)
	ln, err := agwpeserver.Serve(eng, b, cfg.Server.ListenHost, cfg.Server.ListenPort, cfg.Server.AllowedSubnets)
	if err != nil {
		return nil, fmt.Errorf("agwpe server: %w", err)
	}

	r := &Runtime{eng: eng, bridge: b, agwpeLn: ln}

	if cfg.KISSTCP.Enabled {
		warnIfExposed("kisstcp", cfg.KISSTCP.ListenHost, cfg.KISSTCP.ListenPort, cfg.KISSTCP.AllowedSubnets)
		r.kissSrv, err = kisstcpserver.Serve(eng, b, cfg.KISSTCP.ListenHost, cfg.KISSTCP.ListenPort, cfg.KISSTCP.MaxClients, cfg.KISSTCP.IdleTimeout, cfg.KISSTCP.AllowedSubnets)
		if err != nil {
			ln.Close()
			return nil, fmt.Errorf("kisstcp server: %w", err)
		}
		slog.Info("KISS-over-TCP passthrough started",
			"listen", fmt.Sprintf("%s:%d", cfg.KISSTCP.ListenHost, cfg.KISSTCP.ListenPort))
	}

	if cfg.API.Enabled {
		warnIfExposed("api", cfg.API.ListenHost, cfg.API.ListenPort, cfg.API.AllowedSubnets)
		r.apiSrv, err = apiserver.Serve(eng, b, cfg.API.ListenHost, cfg.API.ListenPort, cfg.API.MaxClients, cfg.API.ServeUI, cfg.API.AllowedSubnets, cfg.API.AllowedHosts...)
		if err != nil {
			if r.kissSrv != nil {
				r.kissSrv.Close()
			}
			ln.Close()
			return nil, fmt.Errorf("api server: %w", err)
		}
		slog.Info("monitoring API started",
			"listen", fmt.Sprintf("%s:%d", cfg.API.ListenHost, cfg.API.ListenPort))
	}

	// One rigctl listener per port with [rigctl.N].enabled = true. Disabled
	// (the default) starts nothing for that port -- rig control is opt-in.
	for i, rc := range cfg.RigCtl {
		if !rc.Enabled {
			continue
		}
		port := i
		warnIfExposed(fmt.Sprintf("rigctl.%d", port), rc.ListenHost, rc.ListenPort, rc.AllowedSubnets)
		// provider is called by a rigctl connection goroutine, never the
		// engine loop -- see rigForPort's doc comment for why it's safe for
		// it to block on the round trip to the loop and back.
		provider := func() (rigctl.Rig, error) { return r.rigForPort(port) }
		srv := rigctl.New(rc, provider)
		if err := srv.Start(); err != nil {
			r.closeRigServers()
			if r.apiSrv != nil {
				r.apiSrv.Close()
			}
			if r.kissSrv != nil {
				r.kissSrv.Close()
			}
			ln.Close()
			return nil, fmt.Errorf("rigctl server (port %d): %w", port, err)
		}
		r.rigSrvs = append(r.rigSrvs, srv)
		slog.Info("rigctl listener started", "port", port, "listen", srv.Addr())
	}

	return r, nil
}

// rigForPort resolves the *rig.Rig currently bound to port's control
// channel, if any, as a rigctl.Server provider. It is called from a rigctl
// connection goroutine (never the engine loop -- see internal/frontend/rigctl's
// Server.handleConn, which explicitly runs off-loop) and does one quick round
// trip onto the engine loop and back: bridge.RigFor reads engine-owned state
// (b.ports) and must run there, but it does no radio I/O itself -- that
// happens later, off-loop, on whatever *rig.Rig this returns. Blocking this
// caller on <-done is therefore bounded by "how long it takes the engine to
// reach the front of its queue and run one slice lookup", not by any radio
// round trip.
//
// On error this returns (nil, err) -- an untyped nil, so the rigctl.Rig
// interface value is genuinely nil, not a typed-nil *rig.Rig wrapped in a
// non-nil interface. That distinction is exactly the footgun
// rigctl.rigIsNil's doc comment warns about: assigning a nil *rig.Rig to the
// interface return (e.g. "return rg, err" with rg still nil) would produce a
// non-nil interface whose first method call panics.
func (r *Runtime) rigForPort(port int) (rigctl.Rig, error) {
	var (
		rg  *rig.Rig
		err error
	)
	done := make(chan struct{})
	r.eng.Do(func() {
		rg, err = r.bridge.RigFor(port)
		close(done)
	})
	<-done
	if err != nil {
		return nil, err
	}
	return rg, nil
}

// rigCtlListenerCount reports how many rigctl listeners are currently
// running. Test-only introspection (see rigctl_test.go).
func (r *Runtime) rigCtlListenerCount() int { return len(r.rigSrvs) }

// closeRigServers closes every running rigctl listener and returns once all
// of them have. Shared by New's startup-failure cleanup path and Shutdown.
func (r *Runtime) closeRigServers() {
	for _, s := range r.rigSrvs {
		s.Close()
	}
}

// warnIfExposed logs a prominent warning when an unauthenticated listener
// binds a non-loopback address. tncd's protocols (AGWPE, KISS-over-TCP) have
// no authentication by design: anyone who can reach the port can transmit
// under an arbitrary callsign on the operator's station.
func warnIfExposed(name, host string, port int, allow netutil.Allowlist) {
	exposed := true
	switch {
	case strings.EqualFold(host, "localhost"):
		exposed = false
	default:
		if ip, err := netip.ParseAddr(host); err == nil && ip.IsLoopback() {
			exposed = false
		}
	}
	if !exposed {
		return
	}
	addr := fmt.Sprintf("%s:%d", host, port)
	if allow.Enabled() {
		slog.Info("listener reachable beyond this host but restricted by allowed_subnets",
			"listener", name, "addr", addr)
		return
	}
	slog.Warn("UNAUTHENTICATED listener reachable beyond this host — anyone who can reach it can transmit under your callsign; restrict with allowed_subnets or a firewall",
		"listener", name, "addr", addr)
}

// AGWPEAddr returns the address the AGWPE server is listening on. Useful when
// the configured port is 0 (ephemeral) and for status/manage displays.
func (r *Runtime) AGWPEAddr() net.Addr { return r.agwpeLn.Addr() }

// Wait runs the engine loop on the calling goroutine, blocking until Shutdown
// completes its teardown (which stops the loop).
func (r *Runtime) Wait() { r.eng.Run() }

// Shutdown runs the graceful teardown sequence:
//  0. close the rigctl listeners (releasing any keyed PTT while ports are
//     still alive),
//  1. close AGWPE client transports (so the listener's Accept unblocks),
//  2. close the listeners (AGWPE, KISS-over-TCP, API),
//  3. bridge.Shutdown() (KISS exit strings + port close),
//  4. engine.Stop().
//
// Step 0 runs before, and outside, the eng.Do below on purpose, for two
// reasons that both matter:
//
//   - Ordering: rigctl.Server.Close force-releases a keyed PTT (see its doc
//     comment), which needs the port's control channel to still be alive to
//     send the un-key command. bridge.Shutdown (step 3) closes the ports'
//     transports, so it must not run first — closing the rigctl servers
//     before touching the bridge at all is what guarantees that, rather than
//     leaving it to scheduling luck.
//   - Deadlock avoidance: closing a rigctl server calls its provider
//     (rigForPort), which itself posts to and blocks on <-done from the
//     engine loop. Doing that FROM inside an eng.Do closure — i.e. from the
//     engine loop's own goroutine — would block that goroutine waiting on
//     work it alone is responsible for draining, forever.
//
// Safe to call from any goroutine.
func (r *Runtime) Shutdown() {
	r.closeRigServers()

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
