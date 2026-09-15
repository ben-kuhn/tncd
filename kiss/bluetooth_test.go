package kiss

import "testing"

func TestParseSPPChannel(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		channel int
		pinned  bool
		wantErr bool
	}{
		{name: "empty means SDP discovery", in: "", channel: 0, pinned: false},
		{name: "whitespace only means SDP discovery", in: "   ", channel: 0, pinned: false},
		{name: "TNC4 channel 1", in: "1", channel: 1, pinned: true},
		{name: "TNC3 channel 6", in: "6", channel: 6, pinned: true},
		{name: "surrounding whitespace tolerated", in: " 6 ", channel: 6, pinned: true},
		{name: "upper bound 30", in: "30", channel: 30, pinned: true},
		{name: "zero rejected", in: "0", wantErr: true},
		{name: "negative rejected", in: "-1", wantErr: true},
		{name: "above 30 rejected", in: "31", wantErr: true},
		{name: "non-numeric rejected", in: "auto", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			channel, pinned, err := parseSPPChannel(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseSPPChannel(%q) = (%d, %v, nil), want error", tt.in, channel, pinned)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSPPChannel(%q) returned unexpected error: %v", tt.in, err)
			}
			if channel != tt.channel || pinned != tt.pinned {
				t.Errorf("parseSPPChannel(%q) = (%d, %v), want (%d, %v)",
					tt.in, channel, pinned, tt.channel, tt.pinned)
			}
		})
	}
}
