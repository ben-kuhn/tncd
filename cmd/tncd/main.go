package main

import (
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/ben-kuhn/tncd/v2/internal/bridge"
	"github.com/ben-kuhn/tncd/v2/internal/config"
	"github.com/ben-kuhn/tncd/v2/internal/engine"
	agwpeserver "github.com/ben-kuhn/tncd/v2/internal/frontend/agwpe"
	apiserver "github.com/ben-kuhn/tncd/v2/internal/frontend/api"
	kisstcpserver "github.com/ben-kuhn/tncd/v2/internal/frontend/kisstcp"
	"github.com/ben-kuhn/tncd/v2/internal/version"
)

// verboseCounter is a custom flag.Value that counts how many times -v appears.
type verboseCounter struct {
	n int
}

func (c *verboseCounter) String() string { return fmt.Sprintf("%d", c.n) }
func (c *verboseCounter) Set(_ string) error {
	c.n++
	return nil
}
func (c *verboseCounter) IsBoolFlag() bool { return true }

// expandCountFlags pre-processes os.Args so that collapsed count flags like
// -vvv or -tt are expanded to multiple single flags (-v -v -v, -t -t).
// countNames is the set of single-letter flag names that use verboseCounter.
// All other args (long flags, values, subcommands) are left untouched.
func expandCountFlags(args []string, countNames map[string]bool) []string {
	out := make([]string, 0, len(args)+4)
	for _, arg := range args {
		// Must start with '-' but not '--' to be a short flag cluster.
		if !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "--") {
			out = append(out, arg)
			continue
		}
		body := arg[1:] // strip leading '-'
		// A pure count cluster is entirely composed of count-flag letters, e.g. "vvv" or "tt".
		// If any character is not in countNames, or if the cluster contains mixed names,
		// leave the arg untouched so stdlib flag can handle it.
		if len(body) < 2 {
			// Single-char short flag — nothing to expand.
			out = append(out, arg)
			continue
		}
		// Check whether the cluster is all the same count-flag letter (e.g. vvv, tt).
		// Mixed clusters like -vt are not expanded (unusual, leave to flag parser).
		first := string(body[0])
		if !countNames[first] {
			out = append(out, arg)
			continue
		}
		allSame := true
		for _, ch := range body[1:] {
			if string(ch) != first {
				allSame = false
				break
			}
		}
		if !allSame {
			out = append(out, arg)
			continue
		}
		// Expand: -vvv → -v -v -v
		for range body {
			out = append(out, "-"+first)
		}
	}
	return out
}

func main() {
	// Check for subcommands before flag parsing.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version":
			fmt.Println("tncd", version.Version)
			return
		case "genconfig":
			fmt.Print(config.Example())
			return
		case "check":
			runCheck(os.Args[2:])
			return
		}
	}

	// Pre-expand collapsed count flags (-vvv → -v -v -v, -tt → -t -t).
	countNames := map[string]bool{"v": true, "t": true}
	expandedArgs := expandCountFlags(os.Args[1:], countNames)

	// --- Flag parsing ---
	fs := flag.NewFlagSet("tncd", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: tncd [options]\n\n")
		fmt.Fprintf(os.Stderr, "AGWPE-to-KISS Translation Bridge\n\n")
		fmt.Fprintf(os.Stderr, "Subcommands:\n")
		fmt.Fprintf(os.Stderr, "  version         Print version and exit\n")
		fmt.Fprintf(os.Stderr, "  genconfig       Print example configuration and exit\n")
		fmt.Fprintf(os.Stderr, "  check -c FILE   Validate configuration file and exit\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		fs.PrintDefaults()
	}

	configFile := fs.String("c", "", "Configuration file (INI format)")
	fs.String("config", "", "Configuration file (INI format) (long form of -c)")

	// Server options
	listenHost := fs.String("listen-host", "", "AGWPE server listen address (default: 127.0.0.1)")
	listenPort := fs.Int("listen-port", 0, "AGWPE server listen port (default: 8000)")
	callsign := fs.String("callsign", "", "Callsign for AGWPE responses (default: AGWPE)")

	// KISS client options
	kissType := fs.String("kiss-type", "", "KISS connection type: serial or tcp")
	kissDevice := fs.String("kiss-device", "", "Serial device (e.g., /dev/ttyUSB0)")
	kissHost := fs.String("kiss-host", "", "KISS TCP host")
	kissPort := fs.Int("kiss-port", 0, "KISS TCP port")
	baudrate := fs.Int("b", 0, "Serial baud rate (default: 9600)")
	fs.Int("baudrate", 0, "Serial baud rate (long form of -b)")
	otaBaudrate := fs.Int("ota-baudrate", 0, "Over-the-air baud rate for T1 calculation (default: 1200)")

	// Verbosity: custom counter flags.
	// -v / --verbose count AX.25 frame verbosity; -t / --traffic-debug count hex dump verbosity.
	vCount := &verboseCounter{}
	fs.Var(vCount, "v", "-v: AX.25 frame type/src/dst; -vv: also data; -vvv: AGWPE detail")
	fs.Var(vCount, "verbose", "Verbose (long form of -v; repeatable)")

	tCount := &verboseCounter{}
	fs.Var(tCount, "t", "Enable raw hex dumps (use -tt for more detail) (short form of --traffic-debug)")
	fs.Var(tCount, "traffic-debug", "Enable raw hex dumps")

	logLevel := fs.String("log-level", "", "Log level: debug, info, warn, error")

	if err := fs.Parse(expandedArgs); err != nil {
		os.Exit(1)
	}

	// -config is a long alias for -c
	if longConfig := fs.Lookup("config").Value.String(); longConfig != "" && *configFile == "" {
		*configFile = longConfig
	}
	// --baudrate is a long alias for -b
	if longBaud := fs.Lookup("baudrate").Value.String(); longBaud != "0" && *baudrate == 0 {
		fmt.Sscanf(longBaud, "%d", baudrate)
	}

	// --- Logging setup ---
	var level slog.Level
	switch *logLevel {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		// -vvv implies debug
		if vCount.n >= 3 {
			level = slog.LevelDebug
		} else {
			level = slog.LevelInfo
		}
	}

	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(handler))
	// Also configure the stdlib log package to go through slog (bridge/agwpe use log.Printf).
	log.SetFlags(0)
	log.SetOutput(os.Stderr)

	// --- Load config ---
	cfg, err := config.Load(*configFile)
	if err != nil {
		slog.Error("config load failed", "err", err)
		os.Exit(1)
	}

	// --- Apply CLI overrides (mirrors tncd.py:2278-2295) ---
	if *listenHost != "" {
		cfg.Server.ListenHost = *listenHost
	}
	if *listenPort != 0 {
		cfg.Server.ListenPort = *listenPort
	}
	if *callsign != "" {
		cfg.Server.Callsign = *callsign
	}
	if len(cfg.Ports) > 0 {
		if *kissType != "" {
			cfg.Ports[0].Type = *kissType
		}
		if *kissDevice != "" {
			cfg.Ports[0].Device = *kissDevice
		}
		if *kissHost != "" {
			cfg.Ports[0].Host = *kissHost
		}
		if *kissPort != 0 {
			cfg.Ports[0].TCPPort = *kissPort
		}
		if *baudrate != 0 {
			cfg.Ports[0].SerialBaudrate = *baudrate
		}
		if *otaBaudrate != 0 {
			cfg.Ports[0].OTABaudrate = *otaBaudrate
		}
	}

	if err := cfg.Validate(); err != nil {
		slog.Error("config validation failed", "err", err)
		os.Exit(1)
	}

	// --- Build runtime ---
	eng := engine.New()
	b := bridge.New(eng, cfg)
	b.SetVerbosity(vCount.n, tCount.n)

	if err := b.Start(); err != nil {
		slog.Error("bridge start failed", "err", err)
		os.Exit(1)
	}

	b.RegisterMonitorSink(agwpeserver.NewMonitorSink(b))

	ln, err := agwpeserver.Serve(eng, b, cfg.Server.ListenHost, cfg.Server.ListenPort)
	if err != nil {
		slog.Error("agwpe server failed to start", "err", err)
		os.Exit(1)
	}

	var kissSrv *kisstcpserver.Server
	if cfg.KISSTCP.Enabled {
		kissSrv, err = kisstcpserver.Serve(eng, b, cfg.KISSTCP.ListenHost, cfg.KISSTCP.ListenPort, cfg.KISSTCP.MaxClients)
		if err != nil {
			slog.Error("kisstcp server failed to start", "err", err)
			os.Exit(1)
		}
		slog.Info("KISS-over-TCP passthrough started",
			"listen", fmt.Sprintf("%s:%d", cfg.KISSTCP.ListenHost, cfg.KISSTCP.ListenPort))
	}

	var apiSrv *apiserver.Server
	if cfg.API.Enabled {
		apiSrv, err = apiserver.Serve(eng, b, cfg.API.ListenHost, cfg.API.ListenPort, cfg.API.MaxClients)
		if err != nil {
			slog.Error("api server failed to start", "err", err)
			os.Exit(1)
		}
		slog.Info("read-only API started",
			"listen", fmt.Sprintf("%s:%d", cfg.API.ListenHost, cfg.API.ListenPort))
	}

	slog.Info("tncd running", "version", version.Version,
		"listen", fmt.Sprintf("%s:%d", cfg.Server.ListenHost, cfg.Server.ListenPort))
	slog.Info("Press Ctrl+C to stop")

	// --- Signal handling ---
	// Wire shutdown: SIGINT / SIGTERM → post shutdown sequence to engine loop.
	// Ordering mirrors tncd.py:1167-1184:
	//   1. Close AGWPE client transports (so the listener's Accept returns when closed).
	//   2. Close the listener.
	//   3. bridge.Shutdown() (ExitKISS + port close).
	//   4. engine.Stop().
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		slog.Info("Received signal, shutting down...", "signal", sig)
		eng.Do(func() {
			// Step 1: close AGWPE client transports before closing the listener.
			// Without this, ln.Close() races with active client goroutines that
			// call conn.Write; more importantly, the server goroutine would block
			// on open connections after Accept returns an error.
			for _, c := range b.Clients() {
				c.CloseTransport()
			}

			// Step 2: close the listener (stops accepting new clients).
			ln.Close()

			// Step 2b: close the KISS-over-TCP server (listener + clients).
			if kissSrv != nil {
				kissSrv.Close()
			}

			// Step 2c: close the read-only API server.
			if apiSrv != nil {
				apiSrv.Close()
			}

			// Step 3: graceful port shutdown (sends KISS exit, closes serial/TCP).
			b.Shutdown()

			// Step 4: stop the engine loop.
			eng.Stop()
		})
	}()

	// Run the engine on the main goroutine (blocks until eng.Stop() is called).
	eng.Run()
	slog.Info("tncd stopped")
}

// runCheck implements the "check" subcommand: load + validate a config file
// and print "OK" or the error. Exits 0/1.
func runCheck(args []string) {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	cfgFile := fs.String("c", "", "Configuration file to check")
	fs.String("config", "", "Configuration file to check (long form)")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	// Support --config as alias for -c
	if longCfg := fs.Lookup("config").Value.String(); longCfg != "" && *cfgFile == "" {
		*cfgFile = longCfg
	}

	if *cfgFile == "" {
		fmt.Fprintln(os.Stderr, "check: -c FILE is required")
		os.Exit(1)
	}

	cfg, err := config.Load(*cfgFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("OK")
}
