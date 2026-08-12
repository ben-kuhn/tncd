//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"github.com/ben-kuhn/tncd/v2/internal/ports"
	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
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

// hideConsole hides the console window Explorer flashed for a double-click, so
// only the GUI is visible. (tncd stays a console-subsystem app so the CLI and
// service paths keep working normally.)
func hideConsole() {
	if h := win.GetConsoleWindow(); h != 0 {
		win.ShowWindow(h, win.SW_HIDE)
	}
}

// runGUI shows the manage window if tncd is already installed, else the
// installer wizard.
func runGUI() {
	var err error
	if isInstalled() {
		err = manageWindow()
	} else {
		err = installerWizard()
	}
	if err != nil {
		// No console in GUI mode — leave a breadcrumb for diagnosis.
		if f, e := os.Create(filepath.Join(os.TempDir(), "tncd-gui.log")); e == nil {
			fmt.Fprintf(f, "tncd GUI error: %v\n", err)
			f.Close()
		}
	}
}

// isInstalled reports whether the tncd service is registered or the program is
// present under %ProgramFiles%.
func isInstalled() bool {
	if m, err := mgr.Connect(); err == nil {
		if s, err := m.OpenService(serviceName); err == nil {
			s.Close()
			m.Disconnect()
			return true
		}
		m.Disconnect()
	}
	exeDir, _ := installDirs()
	if _, err := os.Stat(filepath.Join(exeDir, "tncd.exe")); err == nil {
		return true
	}
	return false
}

// portChoice is a TNC the wizard can install: a serial device (usb: ref / COMx)
// or a Bluetooth device (MAC). Bluetooth entries are added in a later sub-plan.
type portChoice struct {
	typ    string // "serial" | "bluetooth"
	device string // serial: usb:VID:PID / COMx ;  bluetooth: MAC
}

func wizardPorts() ([]string, []portChoice) {
	ps, _ := ports.List()
	labels := make([]string, 0, len(ps))
	choices := make([]portChoice, 0, len(ps))
	for _, p := range ps {
		labels = append(labels, p.Label)
		if p.Kind == ports.KindBluetooth {
			choices = append(choices, portChoice{typ: "bluetooth", device: p.Device})
		} else {
			choices = append(choices, portChoice{typ: "serial", device: p.Ref})
		}
	}
	return labels, choices
}

func buildConfig(callsign string, ch portChoice) string {
	var b strings.Builder
	b.WriteString("[server]\r\nlisten_host = 127.0.0.1\r\nlisten_port = 8000\r\ncallsign = ")
	b.WriteString(callsign)
	b.WriteString("\r\n\r\n[api]\r\nenabled = true\r\nlisten_host = 127.0.0.1\r\nlisten_port = 8002\r\n\r\n")
	b.WriteString("[client.0]\r\ntype = ")
	b.WriteString(ch.typ)
	b.WriteString("\r\n")
	if ch.typ == "bluetooth" {
		b.WriteString("bdaddr = " + ch.device + "\r\n")
	} else {
		b.WriteString("device = " + ch.device + "\r\n")
	}
	b.WriteString("reconnect = true\r\n")
	return b.String()
}

func installerWizard() error {
	labels, choices := wizardPorts()
	var callsignLE *walk.LineEdit
	var portCB *walk.ComboBox
	var mw *walk.MainWindow

	doInstall := func() {
		callsign := strings.ToUpper(strings.TrimSpace(callsignLE.Text()))
		if callsign == "" {
			walk.MsgBox(mw, "tncd", "Please enter your callsign.", walk.MsgBoxIconWarning)
			return
		}
		idx := portCB.CurrentIndex()
		if idx < 0 || idx >= len(choices) {
			walk.MsgBox(mw, "tncd", "Please select a TNC.", walk.MsgBoxIconWarning)
			return
		}
		tmp := filepath.Join(os.TempDir(), "tncd-setup.ini")
		if err := os.WriteFile(tmp, []byte(buildConfig(callsign, choices[idx])), 0o644); err != nil {
			walk.MsgBox(mw, "tncd", "Could not write config: "+err.Error(), walk.MsgBoxIconError)
			return
		}
		code, err := elevate(`install -c "` + tmp + `"`)
		if err != nil {
			walk.MsgBox(mw, "tncd", "Install did not run: "+err.Error(), walk.MsgBoxIconError)
			return
		}
		if code != 0 {
			walk.MsgBox(mw, "tncd", fmt.Sprintf("Install failed (exit %d). Check permissions and the config.", code), walk.MsgBoxIconError)
			return
		}
		_, cfgDir := installDirs()
		walk.MsgBox(mw, "tncd installed",
			"tncd is installed and running as a Windows service.\r\n\r\n"+
				"Config:      "+filepath.Join(cfgDir, "tncd.ini")+"\r\n"+
				"Web monitor: http://127.0.0.1:8002\r\n\r\n"+
				"Bluetooth TNC? Pair it in Windows Settings, then set bdaddr in the config.",
			walk.MsgBoxIconInformation)
		mw.Close()
	}

	_, err := MainWindow{
		AssignTo: &mw,
		Title:    "tncd Setup",
		MinSize:  Size{Width: 480, Height: 300},
		Layout:   VBox{},
		Children: []Widget{
			Label{Text: "Install tncd as a Windows service.\r\nChoose your callsign and TNC, then click Install."},
			Composite{
				Layout: Grid{Columns: 2},
				Children: []Widget{
					Label{Text: "Callsign:"},
					LineEdit{AssignTo: &callsignLE},
					Label{Text: "TNC:"},
					ComboBox{AssignTo: &portCB, Model: labels, Editable: false},
				},
			},
			Label{Text: "No serial TNC listed? Plug it in and reopen. Bluetooth TNCs are added after pairing."},
			VSpacer{},
			Composite{
				Layout: HBox{},
				Children: []Widget{
					HSpacer{},
					PushButton{Text: "Install", OnClicked: doInstall},
					PushButton{Text: "Cancel", OnClicked: func() { mw.Close() }},
				},
			},
		},
	}.Run()
	return err
}

func manageWindow() error {
	_, cfgDir := installDirs()
	cfgPath := filepath.Join(cfgDir, "tncd.ini")
	var mw *walk.MainWindow

	_, err := MainWindow{
		AssignTo: &mw,
		Title:    "tncd",
		MinSize:  Size{Width: 360, Height: 280},
		Layout:   VBox{},
		Children: []Widget{
			Label{Text: "tncd is installed as a Windows service."},
			PushButton{Text: "Start service", OnClicked: func() { _, _ = elevate("service start") }},
			PushButton{Text: "Stop service", OnClicked: func() { _, _ = elevate("service stop") }},
			PushButton{Text: "Open config", OnClicked: func() { openPath(cfgPath) }},
			PushButton{Text: "Open web monitor", OnClicked: func() { openPath("http://127.0.0.1:8002") }},
			VSpacer{},
			PushButton{
				Text: "Uninstall tncd",
				OnClicked: func() {
					if walk.MsgBox(mw, "tncd", "Uninstall tncd? The config is kept for reinstalls.", walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) == walk.DlgCmdYes {
						_, _ = elevate("uninstall")
						mw.Close()
					}
				},
			},
			PushButton{Text: "Close", OnClicked: func() { mw.Close() }},
		},
	}.Run()
	return err
}
