//go:build windows

package kiss

import "testing"

func TestParseBTAddr(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
		ok   bool
	}{
		{"00:11:22:33:44:55", 0x001122334455, true},
		{"AA:BB:CC:DD:EE:FF", 0xAABBCCDDEEFF, true},
		{"aa-bb-cc-dd-ee-ff", 0xAABBCCDDEEFF, true}, // dashes + lowercase
		{"001122334455", 0x001122334455, true},       // no separators
		{"00:11:22:33:44", 0, false},                 // too short
		{"zz:11:22:33:44:55", 0, false},              // non-hex
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
