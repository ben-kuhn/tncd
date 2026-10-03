# Benshi radios dial Hands-Free at your host — stopping it

**Symptom**: a BTech UV-PRO (or Benshi relative) paired for KISS over Bluetooth
SPP shows up as a phantom headset in your audio settings, and tncd logs this on
every single connect:

```
bluetooth: 38:D2:00:01:52:8F already connected, disconnecting first
bluetooth: 38:D2:00:01:52:8F dropped audio profile 0000111e-0000-1000-8000-00805f9b34fb
bluetooth: 38:D2:00:01:52:8F dropped audio profile 0000111f-0000-1000-8000-00805f9b34fb
```

**Why it matters beyond tidiness**: an active audio profile corrupts the
SPP/KISS data channel on these radios -- writes complete at the socket but
frames never reach the TNC. That is why tncd drops audio profiles on every
connect rather than only at startup.

## What is actually happening

The radio is the initiator, not your host. With a UV-PRO sitting idle,
bluetoothd logs:

```
src/profile.c:record_cb()  No SDP records found for Hands-Free Voice gateway
src/profile.c:ext_confirm() Hands-Free unit authorization failure
No matching connection for device
Device is already marked as connected
```

The radio dials **Hands-Free** at the host, treating it as a phone. It
advertises both `0000111e` (Handsfree) and `0000111f` (Handsfree Audio Gateway).

Two consequences worth separating:

1. **Your audio server claims the radio as a headset.** This is fixable, and is
   the part that corrupts KISS.
2. **The device reads "Connected" even when no profile succeeds.** This is NOT
   fixable from the host: the radio opens the ACL before any profile
   negotiation, so a paired, trusted device will always get that far. Tested --
   a device-scoped rule does not prevent it. tncd therefore has to tear the link
   down before dialling SPP, and that teardown is load-bearing: dialling
   ConnectProfile on an already-connected device does not make BlueZ deliver
   NewConnection at all.

So the goal is to stop (1). Do not expect to stop (2).

## Linux with PipeWire / WirePlumber (0.5+)

Withdraw the headset roles host-wide. A2DP is left enabled, so Bluetooth
speakers and headphones keep working; only the hands-free roles the radios abuse
are gone.

`~/.config/wireplumber/wireplumber.conf.d/97-no-hfp.conf`, or system-wide in
`/etc/wireplumber/wireplumber.conf.d/`:

```
monitor.bluez.properties = {
  bluez5.hfphsp-backend = "none"
  bluez5.roles = [ a2dp_sink a2dp_source ]
}
```

Then `systemctl --user restart wireplumber`.

Measured effect on one host: tncd's "dropped audio profile" count went from 2 on
every connect to 0, and the radio stopped appearing as an audio device.

A device-scoped rule (`monitor.bluez.rules` matching
`bluez_card.<ADDR>` with `device.disabled = true`) also stops WirePlumber
claiming that one radio, and is worth using if you do need headsets on the same
host. It is strictly narrower, so prefer it when headsets matter and the
global switch when they do not.

### NixOS

See `services.pipewire.wireplumber.configPackages` in this fleet's
`modules/ham-radio.nix`, which ships the global form above with the tradeoff
documented inline.

## Linux with PulseAudio

Unload the policy module that performs the auto-connect, in
`/etc/pulse/default.pa` (or `~/.config/pulse/default.pa`):

```
### comment out or remove:
# load-module module-bluetooth-policy
```

`module-bluetooth-discover` can stay -- it is the policy module that drives
profile auto-connection.

## Linux with no audio server

Nothing to do for (1). Without an HFP backend registered on D-Bus, BlueZ has
nothing to hand the radio's Hands-Free attempt, so no audio profile is
established. The "Connected" ACL from (2) still happens.

## Windows

Per-device and persistent, which is better than the Linux situation:

1. Settings -> Bluetooth & devices -> Devices -> **More devices and printer
   settings**
2. Right-click the radio -> **Properties** -> **Services** tab
3. Uncheck **Handsfree Telephony** (and **Headset**, if listed)
4. Apply, then re-pair or power-cycle the radio

The Serial Port service must stay checked -- that is the SPP link tncd uses.

## macOS

Not applicable in practice: macOS does not expose Bluetooth SPP to
applications, so tncd does not support Bluetooth transports there. Use a
serial or TCP transport instead.

## Verifying the fix

Start tncd with `-v -v` against the radio and count the profile drops:

```
grep -c "dropped audio profile" tncd.log
```

Zero means nothing is bringing up a headset profile any more. Expect to still
see `already connected, disconnecting first` -- that is consequence (2), and it
is not a fault.
