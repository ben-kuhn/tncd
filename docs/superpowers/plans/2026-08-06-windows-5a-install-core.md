# Windows Support — Plan 5a: Self-Install Core (`tncd install` / `tncd uninstall`)

> Part of the Plan 5 (self-installer) series. This sub-plan is the headless, VM-testable
> install/uninstall mechanics that the GUI wizard (Plan 5b) will call. No GUI, no new deps.

**Goal:** `tncd install -c FILE` copies the binary into `%ProgramFiles%\tncd`, the config into
`%ProgramData%\tncd\tncd.ini`, registers + starts the Windows service pointing at the installed
copy, and writes an Add/Remove Programs entry. `tncd uninstall` reverses it.

**Architecture:** New Windows-only `cmd/tncd/install_windows.go`; a non-Windows stub
`cmd/tncd/install_unix.go`. Reuses the service-management code by refactoring `svcInstall` into a
shared `installServiceAt(exePath, cfgPath)` (so the service can point at the *installed* exe, not
the running one). Add/Remove entry via `golang.org/x/sys/windows/registry` (already vendored).
`main.go` gains `install` / `uninstall` subcommands.

**Validation:** Windows-only → `GOOS=windows` build+vet gate, then the `tncd-test` VM headlessly:
run elevated `tncd install -c ...`, assert exe in Program Files, config in ProgramData, service
Running, Add/Remove entry present; `tncd uninstall` → all gone. (The built-in Administrator is
already elevated, so no UAC prompt in the CLI path; self-elevation is Plan 5b's GUI concern.)

## Global Constraints
- Pure Go, no cgo; `CGO_ENABLED=0` builds linux + windows amd64/arm64.
- No new module deps (uses `x/sys/windows/{svc,mgr,eventlog,registry}`, already present).
- Reuse the existing service install/uninstall logic — do not duplicate `CreateService`/eventlog.
- Install paths: exe `%ProgramFiles%\tncd\tncd.exe`, config `%ProgramData%\tncd\tncd.ini`
  (fallbacks `C:\Program Files` / `C:\ProgramData` if the env vars are empty).
- The installed service points at the **installed** exe + config (absolute paths).

## Tasks

### Task 1 — Refactor service install to accept explicit exe+config paths
- Modify `cmd/tncd/service_windows.go`: extract `installServiceAt(exePath, cfgPath string) error`
  (the `mgr.Connect` + already-exists check + `CreateService(serviceName, exePath, cfg, "-c",
  cfgPath)` + `eventlog.InstallAsEventCreate` body). `svcInstall(args)` becomes: `absConfigPath` +
  `os.Executable` → `installServiceAt(exe, cfg)`. Also expose `startService()` (wrap
  `svcControl("start")`) and keep `svcUninstall()` (rename-free) reusable.
- Gate: `GOOS=windows go build ./... && go vet ./cmd/tncd/`; Linux unaffected. `service
  install/start/stop/uninstall` behavior unchanged (re-verify on VM: install/start/stop/uninstall
  still `ok`).

### Task 2 — `tncd install` / `tncd uninstall` + dispatch + unix stub
- Create `cmd/tncd/install_windows.go` (`//go:build windows`):
  - `installDirs() (exeDir, cfgDir string)` from `ProgramFiles`/`ProgramData`.
  - `copyFile(src, dst string) error`.
  - `install(srcCfg string) error`: `config.Load`+`Validate` the source config; mkdir dirs;
    copy running exe → `exeDir\tncd.exe`; copy config → `cfgDir\tncd.ini`;
    `installServiceAt(installedExe, installedCfg)`; `startService()`; `writeUninstallEntry(installedExe)`.
  - `uninstall() error`: `svcUninstall()` (best-effort) + `removeUninstallEntry()` +
    best-effort `os.RemoveAll(exeDir)` (may fail if run from the installed exe — ignore).
  - `writeUninstallEntry(exe)`/`removeUninstallEntry()` via
    `registry.LOCAL_MACHINE\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\tncd`
    (DisplayName, DisplayVersion=version.Version, Publisher, InstallLocation,
    `UninstallString = "\"<exe>\" uninstall"`, NoModify=1, NoRepair=1).
  - `runInstall(args) int` (`-c FILE` required) and `runUninstall(args) int`, with clear stdout.
- Create `cmd/tncd/install_unix.go` (`//go:build !windows`): `runInstall`/`runUninstall` print
  "installation is only supported on Windows" and return 1.
- Modify `cmd/tncd/main.go`: add `case "install": os.Exit(runInstall(os.Args[2:]))` and
  `case "uninstall": os.Exit(runUninstall(os.Args[2:]))` to the subcommand switch; add usage lines.
- Gate: build+vet for linux + windows amd64/arm64; Linux `go test ./...`. VM headless validation
  (elevated): `tncd install -c C:\tncd\tncd-api.ini` → assert `%ProgramFiles%\tncd\tncd.exe`,
  `%ProgramData%\tncd\tncd.ini`, `Get-Service tncd` = Running, `Get-ItemProperty HKLM:\...\Uninstall\tncd`
  present; then `tncd uninstall` → service gone + registry key gone.

## Follow-on (later sub-plans)
- **5b** — GUI wizard (`lxn/walk`): double-click detection (installed vs not), welcome →
  callsign + port (from `tncd ports`) → self-elevate (`ShellExecute` `runas`) → write config +
  call `install` → finish; manage window (Start/Stop/Uninstall/Open Config/Open Web Monitor).
  Resolves the console-vs-GUI subsystem question (console app that hides its window for the GUI,
  or GUI app that `AttachConsole`s for the CLI).
- **5c** — Bluetooth device enumeration (`WSALookupService` `NS_BTH`) for the wizard's port list
  (surfaces paired SPP devices as name+MAC; feeds `bdaddr`).
- **5d** — `release.yml` `GOOS=windows` `.exe` artifacts + version resource/manifest (`asInvoker`).
