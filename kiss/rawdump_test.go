package kiss

import (
	"encoding/hex"
	"os"
	"testing"
	"time"
)

// TestRawDumpBluetooth is a hardware diagnostic, not a unit test: it opens a
// Bluetooth SPP transport and dumps every raw read, so the KISS FRAMING on
// the wire can be inspected directly.
//
// The question it exists to answer (docs/followups.md #9): does any TNC emit
// the compact single-delimiter form `C0 <f1> C0 <f2> C0`, where one FEND both
// closes a frame and opens the next? kiss/demux.go does not handle that form
// and would silently drop every second frame from such a peer. Nothing above
// the transport can answer this -- the decoders consume the delimiters -- so
// it has to be read off the raw bytes.
//
// Skipped unless TNCD_HW_BDADDR is set.
func TestRawDumpBluetooth(t *testing.T) {
	addr := os.Getenv("TNCD_HW_BDADDR")
	if addr == "" {
		t.Skip("set TNCD_HW_BDADDR to run the raw Bluetooth dump")
	}
	secs := 45
	tr := NewBluetoothTransport(BluetoothConfig{BDAddr: addr})
	if err := tr.Open(); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer tr.Close()

	t.Logf("capturing raw bytes for %ds -- transmit some frames at it now", secs)

	// Read on a goroutine: Transport.Read blocks indefinitely with no
	// deadline, so polling time.Now() in the read loop never gets a chance to
	// fire when the radio is quiet -- the first attempt at this test hung
	// past its own deadline and was killed by the go test timeout.
	type chunk struct{ b []byte }
	ch := make(chan chunk, 64)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := tr.Read(buf)
			if n > 0 {
				cp := make([]byte, n)
				copy(cp, buf[:n])
				ch <- chunk{cp}
			}
			if err != nil {
				close(ch)
				return
			}
		}
	}()

	var all []byte
	deadline := time.After(time.Duration(secs) * time.Second)
collect:
	for {
		select {
		case c, ok := <-ch:
			if !ok {
				break collect
			}
			all = append(all, c.b...)
			t.Logf("read %3d bytes: %s", len(c.b), hex.EncodeToString(c.b))
		case <-deadline:
			break collect
		}
	}
	if len(all) == 0 {
		t.Fatal("captured nothing -- no frames reached the radio during the window")
	}
	t.Logf("TOTAL %d bytes", len(all))
	t.Logf("FULL STREAM: %s", hex.EncodeToString(all))
	analyseKISSFraming(t, all)
}

// analyseKISSFraming reports how frames are delimited in a raw KISS stream.
func analyseKISSFraming(t *testing.T, b []byte) {
	t.Helper()
	const fend = 0xC0
	var runs []int // length of each run of consecutive FENDs
	i := 0
	single, doubled := 0, 0
	for i < len(b) {
		if b[i] != fend {
			i++
			continue
		}
		j := i
		for j < len(b) && b[j] == fend {
			j++
		}
		runs = append(runs, j-i)
		if j-i == 1 {
			single++
		} else {
			doubled++
		}
		i = j
	}
	t.Logf("FEND runs: %v", runs)
	t.Logf("single-FEND boundaries: %d, multi-FEND boundaries: %d", single, doubled)
	// A stream that is purely `C0 <frame> C0 C0 <frame> C0` has its interior
	// boundaries doubled. Interior single FENDs with data on BOTH sides are
	// the compact form.
	compact := 0
	i = 0
	for i < len(b) {
		if b[i] == fend && i > 0 && i+1 < len(b) && b[i-1] != fend && b[i+1] != fend {
			compact++
		}
		i++
	}
	t.Logf("interior single FENDs with data on both sides (compact-form candidates): %d", compact)
	if compact > 0 {
		t.Logf("=> this peer DOES appear to use compact shared-FEND framing")
	} else {
		t.Logf("=> no compact shared-FEND framing observed from this peer")
	}
}
