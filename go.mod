module github.com/ben-kuhn/tncd/v2

go 1.25.13

require (
	github.com/godbus/dbus/v5 v5.2.2
	// PINNED: kiss/rtscts_linux.go reflects into this library's unexported
	// unixPort.handle field to enable CRTSCTS. Re-test RTSCTS (a real
	// flow-control TNC, e.g. KPC-3+) before upgrading — a field rename makes
	// libSerialFD silently fall back to no flow control.
	go.bug.st/serial v1.8.0
	gopkg.in/ini.v1 v1.67.3
)

require (
	github.com/lxn/walk v0.0.0-20210112085537-c389da54e794
	github.com/lxn/win v0.0.0-20210218163916-a377121e959e
	golang.org/x/sys v0.43.0
)

require gopkg.in/Knetic/govaluate.v3 v3.0.0 // indirect
