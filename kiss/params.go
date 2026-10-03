package kiss

// Params are the per-port KISS-layer settings.
//
// TXDelay..FullDuplex are the KISS TNC timing parameters ([kiss.N] section),
// sent to the TNC at startup; nil fields are left at whatever the TNC already
// has. The remaining fields are used locally by Port and never go on the wire.
type Params struct {
	TXDelay, Persistence, SlotTime, TXTail, FullDuplex *int

	// OTABaud is the on-air bit rate, used to pace TX by air time (see
	// pacing.go). 0 disables pacing, which is the zero-value default.
	OTABaud int
}
