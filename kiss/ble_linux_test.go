//go:build linux

package kiss

import (
	"errors"
	"fmt"
	"testing"

	"github.com/godbus/dbus/v5"
)

// TestIsNoProfilesError: BlueZ answers Device1.Connect() on a GATT-only
// peripheral with "No more profiles to connect to" -- there are no BR/EDR
// profiles to connect. tncd treated that as fatal, so a Mobilinkd TNC4 (which
// advertises LE strongly and works from iOS) could never be opened with
// type = ble at all.
func TestIsNoProfilesError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil is not the no-profiles case", nil, false},
		{"exact BlueZ message", errors.New("No more profiles to connect to"), true},
		{"lowercased", errors.New("no more profiles to connect to"), true},
		{"wrapped", fmt.Errorf("connect: %w", errors.New("No more profiles to connect to")), true},
		{"a real failure is not tolerated", errors.New("Device not available"), false},
		{"in-progress is not tolerated", errors.New("In Progress"), false},
	}
	for _, tc := range cases {
		if got := isNoProfilesError(tc.err); got != tc.want {
			t.Errorf("%s: isNoProfilesError(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// TestAdapterPathFor: forcing an LE link needs the adapter that owns the
// device, derived from the device path rather than assumed to be hci0.
func TestAdapterPathFor(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/org/bluez/hci0/dev_38_D2_00_01_52_8F", "/org/bluez/hci0"},
		{"/org/bluez/hci1/dev_34_81_F4_AA_B3_D3", "/org/bluez/hci1"},
		{"nonsense", "/org/bluez/hci0"},
	}
	for _, tc := range cases {
		if got := string(adapterPathFor(dbus.ObjectPath(tc.in))); got != tc.want {
			t.Errorf("adapterPathFor(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
