//go:build windows

package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ben-kuhn/tncd/v2/internal/config"
	"github.com/ben-kuhn/tncd/v2/internal/version"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
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

// hardenConfigDirACL replaces the config directory's inherited DACL with an
// explicit SYSTEM + Administrators-only one. Default ACL inheritance makes the
// dir admin-write-only in practice but does not guarantee it; a non-admin write
// to tncd.ini would redirect a LocalSystem service, so the installer enforces
// the ACL explicitly. Uses icacls (present since Vista, run from the already-
// elevated installer) rather than raw DACL-building syscalls, which the pinned
// x/sys does not expose.
func hardenConfigDirACL(dir string) error {
	// /inheritance:r drops inherited ACEs; /grant:r gives SYSTEM and
	// Administrators full control, (OI)(CI) propagating to children like
	// tncd.ini. Everyone/Users lose write access.
	args := []string{
		dir,
		"/inheritance:r",
		"/grant:r",
		"SYSTEM:(OI)(CI)F",
		"Administrators:(OI)(CI)F",
	}
	cmd := exec.Command("icacls", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("icacls %s: %v (%s)", dir, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// install copies the running binary and the given config into place, registers
// and starts the service pointing at the installed copies, and writes an
// Add/Remove Programs entry. Requires elevation (Program Files, HKLM, the SCM).
//
// Re-running install over an existing deployment is an upgrade: an installed
// service whose exe is locked by its running process must be stopped so the new
// binary can replace it, then restarted. The service registration itself is
// kept (paths are stable across versions), so no uninstall is required.
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
	// The config dir will hold tncd.ini read by a LocalSystem service; default
	// ACL inheritance is typically admin-write-only in practice but not
	// guaranteed. Enforce an explicit SYSTEM+Administrators-only DACL so a
	// non-admin write to tncd.ini cannot redirect a LocalSystem process.
	if err := hardenConfigDirACL(cfgDir); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not restrict %s ACL: %v\n", cfgDir, err)
	}

	src, err := os.Executable()
	if err != nil {
		return err
	}
	src, _ = filepath.Abs(src)
	destExe := filepath.Join(exeDir, "tncd.exe")
	destCfg := filepath.Join(cfgDir, "tncd.ini")

	// Upgrade path: stop a running tncd service so its exe handle is released
	// and the new binary can be copied over it. Record whether we must restart
	// it (and whether a service exists at all) before touching any file.
	svcExists, wasRunning, err := stopRunningServiceForUpgrade()
	if err != nil {
		return fmt.Errorf("prepare upgrade: %w", err)
	}

	if err := copyFile(src, destExe); err != nil {
		// The exe may still be locked by a non-service process (a console tncd
		// launched from the installed copy, or a service that was slow to stop).
		// Windows permits renaming a running exe, so move the locked file aside
		// (scheduling it for deletion on reboot) and retry with the fresh copy.
		if rerr := replaceLockedExe(destExe); rerr != nil {
			return fmt.Errorf("copy program to %s: %w", destExe, err)
		}
		if err := copyFile(src, destExe); err != nil {
			return fmt.Errorf("copy program to %s: %w", destExe, err)
		}
	}
	if err := copyFile(srcCfg, destCfg); err != nil {
		return fmt.Errorf("copy config to %s: %w", destCfg, err)
	}

	if svcExists {
		// Service already registered and pointing at the stable paths; restart
		// it (if it was running) so the fresh binary takes effect. CreateService
		// would fail on the existing registration, so we skip it entirely.
		if wasRunning {
			if err := startService(); err != nil {
				return fmt.Errorf("restart service: %w", err)
			}
		}
	} else {
		if err := installServiceAt(destExe, destCfg); err != nil {
			return fmt.Errorf("register service: %w", err)
		}
		if err := startService(); err != nil {
			return fmt.Errorf("start service: %w", err)
		}
	}
	if err := writeUninstallEntry(destExe); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write Add/Remove Programs entry: %v\n", err)
	}
	return nil
}

// stopRunningServiceForUpgrade stops the installed tncd service if it is
// running, so its exe file is no longer locked and an upgrade can replace it.
// Returns whether a service is registered at all and whether it was running
// (so the caller knows to restart it after the file swap).
func stopRunningServiceForUpgrade() (exists, wasRunning bool, err error) {
	m, err := mgr.Connect()
	if err != nil {
		return false, false, err
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return false, false, nil // not installed — fresh-install path
	}
	defer s.Close()

	st, err := s.Query()
	if err != nil {
		return true, false, nil // registered but unqueryable; leave it alone
	}
	if st.State != svc.Running && st.State != svc.StartPending {
		return true, false, nil // stopped — nothing to release
	}
	if _, err := s.Control(svc.Stop); err != nil {
		return true, true, fmt.Errorf("stop service %s: %w", serviceName, err)
	}
	// Wait for the process to actually exit so the exe handle is released. If
	// it does not stop in time, the rename fallback in install() still copes.
	deadline := time.Now().Add(20 * time.Second)
	for {
		cur, err := s.Query()
		if err == nil && cur.State == svc.Stopped {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	return true, true, nil
}

// replaceLockedExe moves a running/locked tncd.exe aside so a fresh copy can be
// installed in its place. Windows allows renaming an exe that is in use; the
// renamed file (still held by the old process) is scheduled for deletion on the
// next reboot.
func replaceLockedExe(destExe string) error {
	old := destExe + ".old"
	if err := os.Remove(old); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale %s: %w", old, err)
	}
	if err := os.Rename(destExe, old); err != nil {
		return fmt.Errorf("rename locked exe to %s: %w", old, err)
	}
	scheduleFileDeleteOnReboot(old)
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

// scheduleFileDeleteOnReboot marks a single path for deletion at the next boot
// via MoveFileEx(MOVEFILE_DELAY_UNTIL_REBOOT). Used for files that are still
// locked by a running process (the old exe after an upgrade rename).
func scheduleFileDeleteOnReboot(path string) {
	if w, err := windows.UTF16PtrFromString(path); err == nil {
		_ = windows.MoveFileEx(w, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT)
	}
}

// scheduleDeleteOnReboot marks path (a file, or a directory's exe) for deletion
// at the next boot via MoveFileEx(MOVEFILE_DELAY_UNTIL_REBOOT).
func scheduleDeleteOnReboot(dir string) {
	scheduleFileDeleteOnReboot(filepath.Join(dir, "tncd.exe"))
	scheduleFileDeleteOnReboot(dir)
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
