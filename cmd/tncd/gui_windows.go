//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
	"golang.org/x/sys/windows"
)

// maybeGUI runs the graphical installer / manage UI when tncd.exe was launched
// by a double-click — a bare invocation (no args), interactive (not the SCM),
// on its own freshly-allocated console — rather than from a shell or the
// service. Returns true if it handled the run.
func maybeGUI() bool {
	if len(os.Args) != 1 || isWindowsService() || !ownConsole() {
		return false
	}
	hideConsole()
	runGUI()
	return true
}

var procGetConsoleProcessList = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleProcessList")

// ownConsole reports whether this process is the only one attached to its
// console. Explorer allocates a fresh console for a double-clicked console app
// (count 1); launching from a shell shares the shell's console (count >= 2).
func ownConsole() bool {
	var list [4]uint32
	r, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&list[0])), uintptr(len(list)))
	return r == 1
}

// hideConsole hides the console window that Explorer flashed for a double-click,
// so only the GUI is visible. (tncd stays a console-subsystem app so the CLI and
// service paths keep working normally.)
func hideConsole() {
	if h := win.GetConsoleWindow(); h != 0 {
		win.ShowWindow(h, win.SW_HIDE)
	}
}

// runGUI shows the installer wizard, or the manage window if already installed.
// This is the GUI foundation; the full wizard/manage UI lands next.
func runGUI() {
	var mw *walk.MainWindow
	if _, err := (MainWindow{
		AssignTo: &mw,
		Title:    "tncd Setup",
		MinSize:  Size{Width: 420, Height: 200},
		Layout:   VBox{},
		Children: []Widget{
			Label{Text: "tncd — AGWPE-to-KISS bridge\n\nGUI foundation is working.\nInstaller wizard + manage window land next."},
			PushButton{
				Text:      "Close",
				OnClicked: func() { mw.Close() },
			},
		},
	}.Run()); err != nil {
		// No console in GUI mode — leave a breadcrumb for diagnosis.
		if f, e := os.Create(filepath.Join(os.TempDir(), "tncd-gui.log")); e == nil {
			fmt.Fprintf(f, "tncd GUI failed to start: %v\n", err)
			f.Close()
		}
	}
}
