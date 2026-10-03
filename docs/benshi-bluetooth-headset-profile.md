# Benshi radios dial Hands-Free at your host

**Symptom**: a BTech UV-PRO (or Benshi relative) paired for KISS over Bluetooth
SPP shows up as a phantom headset in your audio settings, and tncd logs this on
every single connect:

```
bluetooth: 38:D2:00:01:52:8F already connected, disconnecting first
bluetooth: 38:D2:00:01:52:8F dropped audio profile 0000111e-0000-1000-8000-00805f9b34fb
bluetooth: 38:D2:00:01:52:8F dropped audio profile 0000111f-0000-1000-8000-00805f9b34fb
```

**Short answer: you do not need to do anything.** Those lines are tncd working
correctly, not a fault. Read on if you want to know why, or if the phantom
headset in your audio UI bothers you enough to want it gone.

## What is happening

The radio is the initiator, not your host. With a UV-PRO sitting idle,
bluetoothd logs:

```
src/profile.c:record_cb()  No SDP records found for Hands-Free Voice gateway
src/profile.c:ext_confirm() Hands-Free unit authorization failure
No matching connection for device
Device is already marked as connected
```

The radio dials **Hands-Free** at the host, treating it as a phone. It
advertises both `0000111e` (Handsfree) and `0000111f` (Handsfree Audio
Gateway), and your audio server takes the bait and claims it as a headset.

This matters beyond tidiness: an active audio profile corrupts the SPP/KISS
data channel on these radios -- writes complete at the socket but frames never
reach the TNC. That is why tncd drops audio profiles on every connect rather
than only at startup, and why it tears the link down before dialling SPP.

## Why there is no clean host-side fix

Two things are happening, and only one of them is preventable.

1. **Your audio server claims the radio as a headset.** Preventable, see below.
2. **The device reads "Connected" even when no profile succeeds.** NOT
   preventable from the host. The radio opens the ACL before any profile
   negotiation, so a paired, trusted device always gets that far. Tested: a
   device-scoped rule stops the audio profile but the ACL comes back within
   10s, every time.

Because (2) cannot be stopped, tncd has to tear the link down before dialling
SPP regardless -- and that teardown is load-bearing, because `ConnectProfile`
on an already-connected device does not make BlueZ deliver `NewConnection` at
all. tncd drops the audio profiles as part of that same teardown.

So suppressing (1) does not remove a step from tncd's connect path or fix
anything that was broken. It silences two log lines per connect and removes the
phantom headset from your audio UI. That is the whole benefit. Decide
accordingly -- the options below are not recommendations.

## If you want the phantom headset gone anyway

### Linux, one radio only (preferred if you use Bluetooth headsets)

A device-scoped WirePlumber rule stops your audio server claiming that one
device, and leaves headset support intact for everything else. In
`~/.config/wireplumber/wireplumber.conf.d/97-no-radio-audio.conf`:

```
monitor.bluez.rules = [
  {
    matches = [ { device.name = "~bluez_card.38_D2_00_01_52_8F" } ]
    actions = { update-props = { device.disabled = true } }
  }
]
```

Substitute your radio's address, underscores not colons. Then
`systemctl --user restart wireplumber`.

### Linux, host-wide (has a real cost -- probably not worth it)

Withdrawing the hands-free roles globally also works, and measurably: it took
tncd's "dropped audio profile" count from 2 on every connect to 0 on a test
host.

```
monitor.bluez.properties = {
  bluez5.hfphsp-backend = "none"
  bluez5.roles = [ a2dp_sink a2dp_source ]
}
```

**But this disables Bluetooth headset and hands-free audio for the whole host,
for every user and every device, to tidy up log output on the radios.** A2DP
sink/source stay enabled so speakers and headphones still work, but anything
needing HFP/HSP -- a headset microphone, a phone call -- stops working. This
was tried in the author's own fleet config and reverted for exactly that
reason. Prefer the device-scoped rule.

On PulseAudio the equivalent is removing `load-module module-bluetooth-policy`
from `/etc/pulse/default.pa`, with the same host-wide cost.

### Windows (per-device, no tradeoff)

Windows exposes this properly, so there is no reason not to do it here:

1. Settings -> Bluetooth & devices -> Devices -> **More devices and printer
   settings**
2. Right-click the radio -> **Properties** -> **Services** tab
3. Uncheck **Handsfree Telephony** (and **Headset**, if listed)
4. Apply, then re-pair or power-cycle the radio

The Serial Port service must stay checked -- that is the SPP link tncd uses.
This affects only that radio, so headsets are unaffected.

### Linux with no audio server

Nothing to do. Without an HFP backend registered on D-Bus, BlueZ has nothing
to hand the radio's Hands-Free attempt, so no audio profile is established. The
"Connected" ACL still happens.

### macOS

Not applicable: macOS does not expose Bluetooth SPP to applications, so tncd
does not support Bluetooth transports there. Use a serial or TCP transport.

## Verifying

```
grep -c "dropped audio profile" tncd.log
```

Zero means nothing is bringing up a headset profile. Expect to still see
`already connected, disconnecting first` either way -- that is consequence (2),
and it is not a fault.

## What would actually fix this

The radio should not advertise or dial hands-free while it is in KISS/SPP use,
and should not present as connected before a profile is negotiated. Both are
in the report filed with the vendor
([2026-10-02](2026-10-02-uvpro-bluetooth-tx-report.md)).
