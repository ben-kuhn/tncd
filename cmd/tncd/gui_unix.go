//go:build !windows

package main

// maybeGUI is a no-op off Windows: there is no graphical installer.
func maybeGUI() bool { return false }
