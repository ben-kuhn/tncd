package main

import (
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"strings"

	"github.com/ben-kuhn/tncd/v2/internal/app"
	"github.com/ben-kuhn/tncd/v2/internal/config"
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
		case "ports":
			os.Exit(runPorts(os.Args[2:]))
		case "service":
			os.Exit(runServiceCommand(os.Args[2:]))
		case "install":
			os.Exit(runInstall(os.Args[2:]))
		case "uninstall":
			os.Exit(runUninstall(os.Args[2:]))
		}
	}

	// Windows double-click (bare, interactive, own console) → graphical
	// installer / manage UI. No-op on other platforms and for shell/service runs.
	if maybeGUI() {
		return
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
		fmt.Fprintf(os.Stderr, "  check -c FILE   Validate configuration file and exit\n")
		fmt.Fprintf(os.Stderr, "  ports [--json]  List serial devices and their stable references\n\n")
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

	service := isWindowsService()
	installLogging(level, service)
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

	// --- Build and run ---
	r, err := app.New(cfg, vCount.n, tCount.n)
	if err != nil {
		slog.Error("startup failed", "err", err)
		os.Exit(1)
	}

	slog.Info("tncd running", "version", version.Version, "listen", r.AGWPEAddr().String())
	if !service {
		// Console/foreground only — meaningless (and misleading in the Event Log)
		// when running under the Windows SCM.
		slog.Info("Press Ctrl+C to stop")
	}

	// Block until shutdown (platform-specific: Unix signals, Windows SCM or
	// console). run performs the graceful teardown before returning.
	run(r, service)
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
