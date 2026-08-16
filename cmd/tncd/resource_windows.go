//go:build windows

package main

// The Windows resource files (resource_windows_amd64.syso, resource_windows_arm64.syso)
// embed the application manifest (Common-Controls v6 for lxn/walk, asInvoker, dpiAware)
// and the VERSIONINFO shown in Explorer's file properties. Regenerate after editing
// tncd.manifest or versioninfo.json:
//
//go:generate go run github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest -platform-specific -manifest tncd.manifest versioninfo.json
//
// then keep only the amd64 + arm64 outputs (the shipped Windows targets).
