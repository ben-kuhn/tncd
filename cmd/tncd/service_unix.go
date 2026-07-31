//go:build !windows

package main

import (
	"fmt"
	"os"
)

// runServiceCommand handles `tncd service ...`. Service management targets the
// Windows Service Control Manager, so it is unsupported off Windows.
func runServiceCommand(_ []string) int {
	fmt.Fprintln(os.Stderr, "service management is only supported on Windows")
	return 1
}
