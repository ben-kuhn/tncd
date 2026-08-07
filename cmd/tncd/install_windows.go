//go:build windows

package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/ben-kuhn/tncd/v2/internal/config"
	"github.com/ben-kuhn/tncd/v2/internal/version"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// uninstallKey is the Add/Remove Programs registry key for tncd.
const uninstallKey = `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\tncd`

// installDirs returns the install locations: the program directory under
// %ProgramFiles% and the config directory under %ProgramData%.
func installDirs() (exeDir, cfgDir string) {
	pf := os.Getenv("ProgramFiles")
	if pf == "" {
		pf = `C:\Program Files`
	}
	pd := os.Getenv("ProgramData")
	if pd == "" {
		pd = `C:\ProgramData`
	}
	return filepath.Join(pf, "tncd"), filepath.Join(pd, "tncd")
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// install copies the running binary and the given config into place, registers
// and starts the service pointing at the installed copies, and writes an
// Add/Remove Programs entry. Requires elevation (Program Files, HKLM, the SCM).
func install(srcCfg string) error {
	// Validate the config before touching anything.
	c, err := config.Load(srcCfg)
	if err != nil {
		return fmt.Errorf("read config %s: %w", srcCfg, err)
	}
	if err := c.Validate(); err != nil {
		return fmt.Errorf("invalid config %s: %w", srcCfg, err)
	}

	exeDir, cfgDir := installDirs()
	if err := os.MkdirAll(exeDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", exeDir, err)
	}
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", cfgDir, err)
	}

	src, err := os.Executable()
	if err != nil {
		return err
	}
	src, _ = filepath.Abs(src)
	destExe := filepath.Join(exeDir, "tncd.exe")
	destCfg := filepath.Join(cfgDir, "tncd.ini")

	if err := copyFile(src, destExe); err != nil {
		return fmt.Errorf("copy program to %s: %w", destExe, err)
	}
	if err := copyFile(srcCfg, destCfg); err != nil {
		return fmt.Errorf("copy config to %s: %w", destCfg, err)
	}

	if err := installServiceAt(destExe, destCfg); err != nil {
		return fmt.Errorf("register service: %w", err)
	}
	if err := startService(); err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	if err := writeUninstallEntry(destExe); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write Add/Remove Programs entry: %v\n", err)
	}
	return nil
}

// uninstall stops and removes the service, removes the Add/Remove entry, and
// deletes the program directory. The config in %ProgramData% is left in place
// for reinstalls. The program exe may be briefly locked by the still-stopping
// service (we retry) or, when uninstalling from the installed copy itself, be
// undeletable while running (we schedule it for deletion on reboot).
func uninstall() error {
	if err := svcUninstall(); err != nil {
		fmt.Fprintf(os.Stderr, "note: %v\n", err)
	}
	if err := removeUninstallEntry(); err != nil {
		fmt.Fprintf(os.Stderr, "note: could not remove Add/Remove entry: %v\n", err)
	}
	exeDir, _ := installDirs()
	var rmErr error
	for i := 0; i < 12; i++ { // the service process may still be exiting (~1-3s)
		if rmErr = os.RemoveAll(exeDir); rmErr == nil {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	// Still locked — schedule the program dir for deletion on the next reboot.
	scheduleDeleteOnReboot(exeDir)
	fmt.Fprintf(os.Stderr, "note: %s is in use; scheduled for removal on next reboot\n", exeDir)
	return nil
}

// scheduleDeleteOnReboot marks path (a file, or a directory's exe) for deletion
// at the next boot via MoveFileEx(MOVEFILE_DELAY_UNTIL_REBOOT).
func scheduleDeleteOnReboot(dir string) {
	for _, p := range []string{filepath.Join(dir, "tncd.exe"), dir} {
		if w, err := windows.UTF16PtrFromString(p); err == nil {
			_ = windows.MoveFileEx(w, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT)
		}
	}
}

func writeUninstallEntry(exe string) error {
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, uninstallKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	_ = k.SetStringValue("DisplayName", "tncd — AGWPE-to-KISS bridge")
	_ = k.SetStringValue("DisplayVersion", version.Version)
	_ = k.SetStringValue("Publisher", "tncd")
	_ = k.SetStringValue("InstallLocation", filepath.Dir(exe))
	_ = k.SetStringValue("UninstallString", fmt.Sprintf("\"%s\" uninstall", exe))
	_ = k.SetDWordValue("NoModify", 1)
	_ = k.SetDWordValue("NoRepair", 1)
	return nil
}

func removeUninstallEntry() error {
	return registry.DeleteKey(registry.LOCAL_MACHINE, uninstallKey)
}

// runInstall implements `tncd install -c FILE`.
func runInstall(args []string) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	cfg := fs.String("c", "", "configuration file to install")
	fs.StringVar(cfg, "config", "", "configuration file to install (long form)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *cfg == "" {
		fmt.Fprintln(os.Stderr, "install: -c FILE is required")
		return 2
	}
	if err := install(*cfg); err != nil {
		fmt.Fprintf(os.Stderr, "install: %v\n", err)
		return 1
	}
	exeDir, cfgDir := installDirs()
	fmt.Printf("tncd installed and started.\n  program: %s\n  config:  %s\n  service: tncd (Automatic)\nEdit the config and restart the service (tncd service stop/start) to change settings.\n",
		filepath.Join(exeDir, "tncd.exe"), filepath.Join(cfgDir, "tncd.ini"))
	return 0
}

// runUninstall implements `tncd uninstall`.
func runUninstall(_ []string) int {
	if err := uninstall(); err != nil {
		fmt.Fprintf(os.Stderr, "uninstall: %v\n", err)
		return 1
	}
	fmt.Println("tncd uninstalled.")
	return 0
}
