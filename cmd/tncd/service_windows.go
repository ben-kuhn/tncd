//go:build windows

package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

const serviceDisplayName = "tncd AGWPE-to-KISS bridge"

// runServiceCommand handles `tncd service install|uninstall|start|stop`.
func runServiceCommand(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: tncd service install -c FILE | uninstall | start | stop")
		return 2
	}
	var err error
	switch args[0] {
	case "install":
		err = svcInstall(args[1:])
	case "uninstall":
		err = svcUninstall()
	case "start":
		err = svcControl("start")
	case "stop":
		err = svcControl("stop")
	default:
		fmt.Fprintf(os.Stderr, "unknown service command %q\n", args[0])
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "service %s: %v\n", args[0], err)
		return 1
	}
	fmt.Printf("service %s: ok\n", args[0])
	return 0
}

// absConfigPath resolves the -c path to an absolute path. A service's working
// directory is C:\Windows\System32, so a relative config path would break; the
// installed service must always carry an absolute path.
func absConfigPath(args []string) (string, error) {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	cfg := fs.String("c", "", "configuration file")
	fs.StringVar(cfg, "config", "", "configuration file (long form)")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if *cfg == "" {
		return "", fmt.Errorf("install requires -c FILE (absolute path recommended)")
	}
	abs, err := filepath.Abs(*cfg)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("config file %s: %w", abs, err)
	}
	return abs, nil
}

func svcInstall(args []string) error {
	cfgPath, err := absConfigPath(args)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return err
	}

	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	if s, err := m.OpenService(serviceName); err == nil {
		s.Close()
		return fmt.Errorf("service %s already exists", serviceName)
	}

	s, err := m.CreateService(serviceName, exe, mgr.Config{
		DisplayName:      serviceDisplayName,
		Description:      "AGWPE-to-KISS bridge for AX.25 packet radio.",
		StartType:        mgr.StartAutomatic,
		DelayedAutoStart: true,
	}, "-c", cfgPath)
	if err != nil {
		return err
	}
	defer s.Close()

	// Register the Event Log source so the service can write to it. Ignore
	// "already exists" from a prior install.
	if err := eventlog.InstallAsEventCreate(serviceName, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil {
		// Non-fatal: log to stderr; the service still runs (falls back to stderr).
		fmt.Fprintf(os.Stderr, "warning: could not register event log source: %v\n", err)
	}
	return nil
}

func svcUninstall() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("service %s is not installed", serviceName)
	}
	defer s.Close()

	// Best-effort stop before delete.
	_, _ = s.Control(svc.Stop)

	if err := s.Delete(); err != nil {
		return err
	}
	if err := eventlog.Remove(serviceName); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove event log source: %v\n", err)
	}
	return nil
}

func svcControl(action string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("service %s is not installed", serviceName)
	}
	defer s.Close()

	switch action {
	case "start":
		return s.Start()
	case "stop":
		_, err := s.Control(svc.Stop)
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}
