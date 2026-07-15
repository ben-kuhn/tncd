package ax25

import "testing"

func TestParseAddress(t *testing.T) {
	cases := []struct {
		in   string
		call string
		ssid uint8
		err  bool
	}{
		{"KU0HN-10", "KU0HN", 10, false},
		{"ku0hn-1", "KU0HN", 1, false}, // normalized uppercase
		{"CQ", "CQ", 0, false},
		{"WIDE1-1", "WIDE1", 1, false},
		{"TOOLONGCALL", "", 0, true}, // > 6 chars
		{"KU0HN-16", "", 0, true},    // SSID > 15
		{"", "", 0, true},
	}
	for _, c := range cases {
		a, err := ParseAddress(c.in)
		if c.err != (err != nil) {
			t.Errorf("ParseAddress(%q) err = %v, want err=%v", c.in, err, c.err)
			continue
		}
		if err == nil && (a.Call != c.call || a.SSID != c.ssid) {
			t.Errorf("ParseAddress(%q) = %s-%d, want %s-%d",
				c.in, a.Call, a.SSID, c.call, c.ssid)
		}
	}
}

func TestAddressString(t *testing.T) {
	if s := (Address{Call: "KU0HN", SSID: 10}).String(); s != "KU0HN-10" {
		t.Errorf("got %q", s)
	}
	if s := (Address{Call: "CQ"}).String(); s != "CQ" {
		t.Errorf("got %q, want CQ (no -0 suffix)", s)
	}
}
