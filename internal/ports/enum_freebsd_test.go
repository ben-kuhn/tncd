//go:build freebsd

package ports

import "testing"

// FreeBSD creates .init and .lock clones beside every serial device. They are
// settings templates, not openable ports, so listing them would offer the
// operator device paths that cannot be used.
func TestFreeBSDCalloutFilter(t *testing.T) {
	accept := []string{"cuau0", "cuaU0", "cuau1", "cuaU12"}
	reject := []string{
		"cuau0.init", "cuau0.lock", "cuaU0.init", "cuaU0.lock",
		"ttyu0", "ttyU0", // dial-in side: blocks on carrier detect
		"cuau", "cua", "null", "random",
	}

	for _, name := range accept {
		if !freebsdCallout.MatchString(name) {
			t.Errorf("freebsdCallout should match callout device %q", name)
		}
	}
	for _, name := range reject {
		if freebsdCallout.MatchString(name) {
			t.Errorf("freebsdCallout should not match %q", name)
		}
	}
}
