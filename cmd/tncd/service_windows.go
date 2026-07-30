//go:build windows

package main

import (
	"fmt"
	"os"
)

// runServiceCommand handles `tncd service install|uninstall|start|stop`.
// The individual operations are implemented in Task 3; this task wires the
// dispatch and usage so the command exists and reports clearly.
func runServiceCommand(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: tncd service install|uninstall|start|stop")
		return 2
	}
	switch args[0] {
	case "install", "uninstall", "start", "stop":
		fmt.Fprintf(os.Stderr, "service %s: not yet implemented\n", args[0])
		return 1
	default:
		fmt.Fprintf(os.Stderr, "unknown service command %q\n", args[0])
		return 2
	}
}
