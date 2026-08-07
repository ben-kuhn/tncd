//go:build !windows

package main

import (
	"fmt"
	"os"
)

// runInstall / runUninstall handle `tncd install` / `tncd uninstall`. The
// self-installer targets the Windows Program Files / service model, so it is
// unsupported off Windows (use the package manager or a manual config there).
func runInstall(_ []string) int {
	fmt.Fprintln(os.Stderr, "installation is only supported on Windows")
	return 1
}

func runUninstall(_ []string) int {
	fmt.Fprintln(os.Stderr, "installation is only supported on Windows")
	return 1
}
