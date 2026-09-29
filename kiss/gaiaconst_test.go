package kiss

import (
	"testing"

	"github.com/ben-kuhn/tncd/v2/benshi"
)

// TestGaiaConstantsMatchBenshi is the compile-time-ish link that followup #3
// asked for.
//
// kiss/demux.go re-declares five of benshi's framing constants rather than
// importing them: the demultiplexer needs byte-level visibility into a frame
// that is only PARTIALLY accumulated, which benshi.Decoder deliberately does
// not expose, and a production kiss -> benshi dependency for five integers
// was judged not worth it. Nothing linked the two copies, so a firmware
// change that moved the frame header would have needed both edited and would
// have failed silently if only one was.
//
// This test is that link. The import is test-only, so the production import
// graph is unchanged, but the values can no longer drift apart unnoticed.
func TestGaiaConstantsMatchBenshi(t *testing.T) {
	for _, c := range []struct {
		name       string
		here, want int
	}{
		{"gaiaStart", int(gaiaStart), benshi.FrameStart},
		{"gaiaVersion", int(gaiaVersion), benshi.FrameVersion},
		{"gaiaHeaderLen", gaiaHeaderLen, benshi.FrameHeaderLen},
		{"gaiaMsgHeaderLen", gaiaMsgHeaderLen, benshi.MsgHeaderLen},
		{"gaiaFlagChecksum", int(gaiaFlagChecksum), int(benshi.FlagChecksum)},
	} {
		if c.here != c.want {
			t.Errorf("kiss.%s = %#x, but benshi says %#x -- the demux's mirrored "+
				"constants have drifted from the protocol definition", c.name, c.here, c.want)
		}
	}
}
