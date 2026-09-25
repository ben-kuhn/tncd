package benshi

import "errors"

// freqMask is the 30-bit frequency field shared by every frequency word in
// this protocol; the top 2 bits carry modulation.
const freqMask = 0x3FFFFFFF

// ErrShortBody reports a reply body too short for its command.
var ErrShortBody = errors.New("benshi: reply body too short")

// ErrRadioRejected reports a reply whose status byte is non-zero: the radio
// understood the command and refused it. Distinct from ErrShortBody, which
// means the reply was truncated -- conflating the two sent operators looking
// at the wrong thing entirely.
var ErrRadioRejected = errors.New("benshi: radio rejected the command")
