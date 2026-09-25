package benshi

// PFEffect is a programmable-function effect id, triggerable remotely
// through DO_PROG_FUNC (CmdDoProgFunc). The full protocol defines many more
// effect ids (volume, squelch, channel up/down, and so on); only the one
// this codebase actually drives is named here, so nothing else is reachable
// by accident.
type PFEffect uint8

// PFEffectMainPTT keys the main-VFO transmitter when sent via DO_PROG_FUNC.
// See (*rig.Rig).SetPTT for the important caveat that this effect carries no
// press/release parameter of its own.
const PFEffectMainPTT PFEffect = 13
