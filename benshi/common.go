package benshi

import "errors"

// freqMask is the 30-bit frequency field shared by every frequency word in
// this protocol; the top 2 bits carry modulation.
const freqMask = 0x3FFFFFFF

// ErrShortBody reports a reply body too short for its command.
var ErrShortBody = errors.New("benshi: reply body too short")
