package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/ben-kuhn/tncd/v2/internal/bridge"
	"github.com/ben-kuhn/tncd/v2/internal/config"
	"github.com/ben-kuhn/tncd/v2/internal/rig"
	"github.com/ben-kuhn/tncd/v2/kiss"
)

// parseRigArgs validates the `tncd rig` verb and its argument.
//
// Frequencies are Hz only. Accepting MHz would make "145.03" and "145030000"
// both plausible, and a silent factor-of-a-million error keys a transmitter on
// the wrong band -- so ParseUint (not ParseFloat) is the whole guard.
func parseRigArgs(args []string) (string, uint32, error) {
	if len(args) == 0 {
		return "", 0, fmt.Errorf("rig: need one of get-freq, set-freq <hz>, teardown, probe")
	}
	switch args[0] {
	case "get-freq", "teardown", "probe":
		return args[0], 0, nil
	case "set-freq":
		if len(args) < 2 {
			return "", 0, fmt.Errorf("rig set-freq: need a frequency in Hz")
		}
		hz, err := strconv.ParseUint(args[1], 10, 32)
		if err != nil {
			return "", 0, fmt.Errorf("rig set-freq: %q is not a frequency in Hz", args[1])
		}
		return "set-freq", uint32(hz), nil
	default:
		return "", 0, fmt.Errorf("rig: unknown command %q", args[0])
	}
}

// buildRigTransport constructs pc's transport via internal/bridge's shared
// constructor. This one-shot CLI and the running bridge must agree on how a
// config.Port turns into a kiss.Transport, so bridge stays the single source
// of truth and this just calls its exported wrapper rather than
// reimplementing the type switch.
func buildRigTransport(pc config.Port) (kiss.Transport, error) {
	return bridge.BuildTransport(pc)
}

// runRig opens port n's transport and runs one Benshi rig command directly
// over it. It is a one-shot tool: it does not start the engine or any
// frontend, and it tears everything back down before returning.
//
// Every error names the port and the link that failed -- config, transport
// open, or the radio's own reply -- because this command's whole reason to
// exist is diagnosing hardware run against real, possibly-misbehaving
// radios. A bare "rig: timeout" tells the operator nothing about which of
// several links died.
func runRig(cfgPath string, port int, args []string) error {
	cmd, hz, err := parseRigArgs(args)
	if err != nil {
		return err
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("rig: could not load config %q: %w", cfgPath, err)
	}
	if port < 0 || port >= len(cfg.Ports) {
		return fmt.Errorf("rig: port %d is not configured (config %q has %d port(s))", port, cfgPath, len(cfg.Ports))
	}

	tr, err := buildRigTransport(cfg.Ports[port])
	if err != nil {
		return fmt.Errorf("rig: port %d: could not build transport: %w", port, err)
	}
	if err := tr.Open(); err != nil {
		return fmt.Errorf("rig: port %d: transport open failed: %w", port, err)
	}
	defer tr.Close()

	// The Benshi (Gaia) command protocol does NOT get a channel of its own on
	// this hardware -- confirmed on a real UV-PRO 2026-09-23. It is served on
	// the SAME RFCOMM connection as KISS traffic ("SPP Dev"); a separate
	// "BS AOC" channel exists, accepts the connection, and answers nothing.
	// A Gaia request written straight to the KISS transport gets a real
	// reply (GET_DEV_INFO round-tripped on the bench), and KISS/Gaia frames
	// were shown to interleave cleanly on the wire (two independent KISS
	// frames plus a Gaia reply, all confirmed on-air by a separate Dire Wolf
	// receiver) -- the two protocols are told apart by their leading bytes
	// (KISS: 0xC0, Gaia: 0xFF 0x01), not by which socket they arrived on.
	// kiss.ControlChannelFor now reflects that: for a Bluetooth transport it
	// hands back a view of tr itself rather than dialling the silent "BS AOC"
	// channel, so this goes through the one real path instead of bypassing
	// it and calling rig.New(tr, ...) directly.
	//
	// This CLI is safe as-is because it is one-shot: nothing else is reading
	// this stream while rig.New's reader loop runs, so there is no framing
	// ambiguity to resolve. A future server-side integration that runs rig
	// control ALONGSIDE a live KISS port on the same transport is a
	// different problem -- it will need a demultiplexer (one reader owning
	// the byte stream, dispatching each frame to the KISS decoder or the
	// rig layer by its leading byte) in front of both consumers. Recorded
	// here rather than left to be rediscovered.
	cc, err := kiss.ControlChannelFor(tr)
	if err != nil {
		return fmt.Errorf("rig: port %d: no rig-control channel: %w", port, err)
	}
	defer cc.Close()

	r := rig.New(cc, 5*time.Second)
	defer r.Close()

	switch cmd {
	case "get-freq":
		got, err := r.GetFreq()
		if err != nil {
			return fmt.Errorf("rig: port %d: get-freq: %w", port, err)
		}
		fmt.Println(got)
	case "set-freq":
		// Not "radio did not answer": SetFreq also refuses on purpose when
		// the radio is on a named memory or in dual watch, and reporting
		// those as a dead link would send the operator hunting the wrong
		// problem. The wrapped error says which it was.
		if err := r.SetFreq(hz); err != nil {
			return fmt.Errorf("rig: port %d: set-freq: %w", port, err)
		}
	case "teardown":
		// Teardown restores what THIS session's first set-freq displaced,
		// and each run of this one-shot command is its own session -- so it
		// only ever has something to restore when a set-freq preceded it in
		// the same invocation, which the CLI has no verb for. Say so plainly
		// rather than exiting 0 on a command that did nothing.
		restored, err := r.Teardown()
		if err != nil {
			return fmt.Errorf("rig: port %d: teardown: %w", port, err)
		}
		if !restored {
			fmt.Println("nothing to restore: no frequency was changed in this invocation " +
				"(use set-freq to tune the radio back by hand)")
		}
	case "probe":
		if err := r.Probe(); err != nil {
			return fmt.Errorf("rig: port %d: probe: radio did not answer: %w", port, err)
		}
		fmt.Println("radio answered")
	}
	return nil
}

// runRigCommand implements the "rig" subcommand: parse -c/--port and the verb,
// run it, and report failures with an exit code. It stays a thin wrapper
// around runRig so runRig itself -- the part with actual behavior -- takes
// plain arguments and is easy to call from a test without touching flag.
func runRigCommand(args []string) int {
	fs := flag.NewFlagSet("rig", flag.ContinueOnError)
	cfgFile := fs.String("c", "", "Configuration file (INI format)")
	fs.String("config", "", "Configuration file (INI format) (long form of -c)")
	port := fs.Int("port", 0, "Port number to control (index into [port.N] sections, default 0)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: tncd rig -c FILE [--port N] get-freq|set-freq HZ|teardown|probe\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if longCfg := fs.Lookup("config").Value.String(); longCfg != "" && *cfgFile == "" {
		*cfgFile = longCfg
	}
	if *cfgFile == "" {
		fmt.Fprintln(os.Stderr, "rig: -c FILE is required")
		return 2
	}

	if err := runRig(*cfgFile, *port, fs.Args()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
