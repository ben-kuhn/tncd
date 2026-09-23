//go:build windows

package kiss

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestParseBTAddr(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
		ok   bool
	}{
		{"00:11:22:33:44:55", 0x001122334455, true},
		{"AA:BB:CC:DD:EE:FF", 0xAABBCCDDEEFF, true},
		{"aa-bb-cc-dd-ee-ff", 0xAABBCCDDEEFF, true}, // dashes + lowercase
		{"001122334455", 0x001122334455, true},      // no separators
		{"00:11:22:33:44", 0, false},                // too short
		{"zz:11:22:33:44:55", 0, false},             // non-hex
	}
	for _, c := range cases {
		got, err := parseBTAddr(c.in)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("parseBTAddr(%q) = %#x, %v; want %#x, nil", c.in, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("parseBTAddr(%q) = %#x, nil; want error", c.in, got)
		}
	}
}

// TestMustUUIDToGUIDByteOrder pins mustUUIDToGUID's byte-order derivation.
//
// This matters more than it looks: Data1/Data2/Data3 are numeric fields but
// Data4 is a verbatim byte tail, so a wrong-endian derivation still produces
// a well-formed GUID that simply never matches the service on the wire --
// and nothing running on Linux can catch that, because this path never
// executes there. This test is the only guard that survives.
func TestMustUUIDToGUIDByteOrder(t *testing.T) {
	// The Benshi control UUID, confirmed live against a UV-PRO (see
	// bluetooth_sdp.go's benshiControlServiceUUID comment for the bench
	// history behind this exact value).
	got := mustUUIDToGUID("39144315-32fa-40db-85ed-fbfeba2d86e6")
	want := windows.GUID{
		Data1: 0x39144315,
		Data2: 0x32fa,
		Data3: 0x40db,
		Data4: [8]byte{0x85, 0xed, 0xfb, 0xfe, 0xba, 0x2d, 0x86, 0xe6},
	}
	if got != want {
		t.Errorf("mustUUIDToGUID(control) = %+v, want %+v", got, want)
	}

	// Mixed-case hex must parse the same as all-lowercase: this is the exact
	// input the pinned sppServiceClassID literal below is Data4-uppercase.
	gotMixed := mustUUIDToGUID("39144315-32FA-40db-85ED-fbfeba2d86e6")
	if gotMixed != want {
		t.Errorf("mustUUIDToGUID(mixed case) = %+v, want %+v", gotMixed, want)
	}

	// The strongest assertion available: derive the well-known SPP UUID and
	// require it to equal the pre-existing sppServiceClassID literal exactly
	// -- a value already proven correct in production. Any byte-order
	// regression in mustUUIDToGUID fails this loudly instead of silently
	// producing a GUID that merely looks plausible.
	gotSPP := mustUUIDToGUID("00001101-0000-1000-8000-00805F9B34FB")
	if gotSPP != sppServiceClassID {
		t.Errorf("mustUUIDToGUID(SPP) = %+v, want sppServiceClassID %+v", gotSPP, sppServiceClassID)
	}
}
