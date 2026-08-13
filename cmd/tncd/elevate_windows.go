//go:build windows

package main

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

const seeMaskNoCloseProcess = 0x00000040

// shellExecuteInfo mirrors SHELLEXECUTEINFOW (64-bit layout).
type shellExecuteInfo struct {
	cbSize         uint32
	fMask          uint32
	hwnd           uintptr
	lpVerb         *uint16
	lpFile         *uint16
	lpParameters   *uint16
	lpDirectory    *uint16
	nShow          int32
	hInstApp       uintptr
	lpIDList       uintptr
	lpClass        *uint16
	hkeyClass      uintptr
	dwHotKey       uint32
	hIconOrMonitor uintptr
	hProcess       uintptr
}

var (
	modShell32         = windows.NewLazySystemDLL("shell32.dll")
	procShellExecuteEx = modShell32.NewProc("ShellExecuteExW")
)

// elevate re-launches this exe elevated (UAC "runas") with the given argument
// string, waits for it, and returns its exit code. Used by the un-elevated GUI
// for install/service actions that need administrator rights.
func elevate(args string) (uint32, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(exe)
	var params *uint16
	if args != "" {
		params, _ = windows.UTF16PtrFromString(args)
	}
	info := shellExecuteInfo{
		fMask:        seeMaskNoCloseProcess,
		lpVerb:       verb,
		lpFile:       file,
		lpParameters: params,
		nShow:        0, // SW_HIDE — the elevated CLI has no window of its own
	}
	info.cbSize = uint32(unsafe.Sizeof(info))

	r, _, e := procShellExecuteEx.Call(uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		return 0, fmt.Errorf("elevation failed or was cancelled: %v", e)
	}
	if info.hProcess == 0 {
		return 0, nil
	}
	h := windows.Handle(info.hProcess)
	defer windows.CloseHandle(h)
	if _, err := windows.WaitForSingleObject(h, windows.INFINITE); err != nil {
		return 0, err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return 0, err
	}
	return code, nil
}

// openPath opens a file, folder, or URL with its default handler.
func openPath(p string) {
	verb, _ := windows.UTF16PtrFromString("open")
	f, _ := windows.UTF16PtrFromString(p)
	_ = windows.ShellExecute(0, verb, f, nil, nil, 1 /* SW_SHOWNORMAL */)
}
