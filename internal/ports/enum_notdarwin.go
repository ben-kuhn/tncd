//go:build !darwin && !freebsd

package ports

import "go.bug.st/serial/enumerator"

// defaultDetailedPorts uses go.bug.st/serial's detailed enumerator, which
// provides USB VID/PID/serial on Linux and Windows without cgo. macOS
// (enum_darwin.go) and FreeBSD (enum_freebsd.go) are handled separately: the
// macOS enumerator needs cgo, and the FreeBSD one is an unimplemented stub
// upstream.
func defaultDetailedPorts() ([]portDetail, error) {
	ds, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return nil, err
	}
	out := make([]portDetail, 0, len(ds))
	for _, d := range ds {
		out = append(out, portDetail{
			Name:    d.Name,
			IsUSB:   d.IsUSB,
			VID:     d.VID,
			PID:     d.PID,
			Serial:  d.SerialNumber,
			Product: d.Product,
		})
	}
	return out, nil
}
