package agwpe

import (
	"bytes"
	"testing"

	"github.com/ben-kuhn/tncd/v2/internal/bridge"
	"github.com/ben-kuhn/tncd/v2/internal/config"
	"github.com/ben-kuhn/tncd/v2/internal/engine"
)

type localCapClient struct {
	capClient
	raw bool
}

// Override RawKISS for localCapClient
func (c *localCapClient) RawKISS() bool {
	return c.raw
}

func TestRawRXSink(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	b := bridge.New(eng, &config.Config{Server: config.Server{MaxClients: 8}})

	rc := &localCapClient{capClient: capClient{mon: false}, raw: true}
	nrc := &localCapClient{capClient: capClient{mon: false}, raw: false}

	done := make(chan struct{})
	eng.Do(func() {
		b.AddClient(rc)
		b.AddClient(nrc)
		close(done)
	})
	<-done

	sink := NewRawRXSink(b)

	// Inject a raw frame
	rawBytes := []byte{0x00, 0x01, 0x02, 0x03}
	sink.OnRawRX(2, rawBytes)

	// rc should have received it
	if rc.n != 1 {
		t.Errorf("rawClient received %d frames, want 1", rc.n)
	}
	if rc.last.kind != 'K' {
		t.Errorf("rawClient kind = %c, want K", rc.last.kind)
	}
	wantData := []byte{0x00, 0x00, 0x01, 0x02, 0x03}
	if !bytes.Equal(rc.last.data, wantData) {
		t.Errorf("rawClient data = %x, want %x", rc.last.data, wantData)
	}

	// nrc should NOT have received it
	if nrc.n != 0 {
		t.Errorf("nonRawClient received %d frames, want 0", nrc.n)
	}
}
