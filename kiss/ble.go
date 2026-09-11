package kiss

// Bluetooth Low Energy KISS TNC transport.
//
// Implements the BLE KISS API published at
// https://github.com/hessu/aprs-specs/blob/master/BLE-KISS-API.md, which is
// what lets one transport serve any conforming radio — the Mobilinkd TNC4 and
// the Benshi/BTech UV-PRO expose the same service, which is why iOS apps
// address them identically.
//
// Why this exists alongside the classic SPP transport: the TX characteristic
// is written *with response*, so the radio acknowledges every write at the ATT
// layer. Classic RFCOMM has no such feedback — its credit-based flow control
// is invisible to the socket API, which is how a link could accept writes for
// minutes while nothing reached the air (see bluetooth_linux.go's TX stall
// detection, which exists only to paper over that gap).

// BLE KISS service and characteristic UUIDs, lowercase to match BlueZ.
const (
	// bleKISSService is advertised by conforming TNCs.
	bleKISSService = "00000001-ba2a-46c9-ae49-01b0961f68bb"
	// bleKISSTXChar accepts KISS data written to the TNC, with response.
	bleKISSTXChar = "00000002-ba2a-46c9-ae49-01b0961f68bb"
	// bleKISSRXChar notifies KISS data coming from the TNC.
	bleKISSRXChar = "00000003-ba2a-46c9-ae49-01b0961f68bb"
)

// bleATTOverhead is the ATT protocol overhead per write: one opcode byte plus
// a two-byte attribute handle. The usable payload is MTU minus this.
const bleATTOverhead = 3

// bleDefaultMTU is the ATT default every LE link starts at, before any MTU
// exchange. Used when the stack cannot report a negotiated value, so a write
// still succeeds (just in smaller pieces) rather than failing outright.
const bleDefaultMTU = 23

// BLEConfig configures a BLE KISS transport.
type BLEConfig struct {
	// BDAddr is the radio's Bluetooth address, "AA:BB:CC:DD:EE:FF".
	BDAddr string
}

// chunkForMTU splits data into pieces that each fit in a single ATT write.
//
// The spec requires both directions to tolerate this: "a KISS frame may span
// multiple BLE transfer units", and a receiver reassembles by scanning the
// byte stream for FEND delimiters. Splitting mid-frame is therefore safe — the
// peer's KISS decoder is already a streaming decoder, as ours is.
func chunkForMTU(data []byte, mtu int) [][]byte {
	if mtu < bleDefaultMTU {
		mtu = bleDefaultMTU
	}
	max := mtu - bleATTOverhead
	if len(data) == 0 {
		return nil
	}
	var out [][]byte
	for len(data) > max {
		out = append(out, data[:max])
		data = data[max:]
	}
	return append(out, data)
}
