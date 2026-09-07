// raw.go — AGWPE raw KISS frame distribution.
// Registered as a bridge.RawRXSink.
package agwpe

import (
	"github.com/ben-kuhn/tncd/v2/internal/bridge"
)

type rawSink struct{ b *bridge.Bridge }

// NewRawRXSink returns a RawRXSink that delivers raw AX.25 frames to AGWPE
// clients that have enabled raw-KISS reception.
func NewRawRXSink(b *bridge.Bridge) bridge.RawRXSink { return &rawSink{b: b} }

func (s *rawSink) OnRawRX(port int, raw []byte) {
	for _, c := range s.b.Clients() {
		if c.RawKISS() {
			payload := make([]byte, len(raw)+1)
			copy(payload[1:], raw)
			c.SendAGWPE(uint8(port), 'K', 0, "", "", payload)
		}
	}
}
