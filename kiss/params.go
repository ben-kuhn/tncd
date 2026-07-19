package kiss

// Params are the KISS TNC timing parameters ([kiss.N] section).
// nil fields are not sent to the TNC.
type Params struct{ TXDelay, Persistence, SlotTime, TXTail, FullDuplex *int }
