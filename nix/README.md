# Nix / NixOS support

> **Beta (2.0 Go line).** The `tncd` package in the overlay now tracks the **2.0
> Go rewrite** (`v1.9x-Beta` tags) — a single static binary built with
> `buildGoModule`, no Python dependencies. It is a **beta** and **not yet as
> thoroughly tested as the stable 1.3.x Python line**. If you need proven
> stability, install a 1.3.x package from `apt.tncd.dev` / `rpm.tncd.dev` instead.

tncd is packaged in [nix-ham-packages](https://github.com/ben-kuhn/nix-ham-packages),
which provides the `tncd` package as a nixpkgs overlay. The 2.0 package is a pure-Go
build (`buildGoModule`, `CGO_ENABLED=0`) with no runtime dependencies.

The NixOS service module is also in
[nix-ham-packages](https://github.com/ben-kuhn/nix-ham-packages) and provides
`services.tncd.*` options and systemd services.

## Quick start (NixOS)

```nix
# configuration.nix
{ config, pkgs, ... }:
let
  ham = builtins.fetchTarball
    "https://github.com/ben-kuhn/nix-ham-packages/archive/main.tar.gz";
in {
  nixpkgs.overlays = [ (import ham) ];
  imports = [ "${ham}/tncd/module.nix" ];

  services.tncd = {
    enable = true;
    settings = {
      server = {
        listen_host = "0.0.0.0";
        listen_port = 8000;
        callsign = "N0CALL";
      };
      "client.0" = {
        type = "serial";
        device = "/dev/ttyUSB0";
        serial_baudrate = 9600;
        ota_baudrate = 1200;
      };
    };
  };
}
```

This creates a `tncd` system user (with `dialout` group access), generates
`/etc/tncd.ini` from `settings`, and starts `tncd.service`.

## Bluetooth TNC

Enable Bluetooth support to pull in `dbus-python` and `PyGObject` and add
the service user to the `bluetooth` group:

```nix
services.tncd = {
  enable = true;
  bluetooth.enable = true;   # adds D-Bus/GLib deps, bluetooth group
  settings = {
    server = {
      listen_host = "0.0.0.0";
      listen_port = 8000;
      callsign = "N0CALL";
    };
    "client.0" = {
      type = "bluetooth";
      bdaddr = "AA:BB:CC:DD:EE:FF";
      ota_baudrate = 1200;
      # channel = 6;            # optional, auto-detected via SDP
      # reconnect = true;       # auto-reconnect (default)
      # reconnect_delay = 5;    # initial delay seconds (default)
      # reconnect_max_delay = 60;  # max delay seconds (default)
    };
  };
};
```

Pair and trust the TNC before starting the service:

```bash
bluetoothctl pair AA:BB:CC:DD:EE:FF
bluetoothctl trust AA:BB:CC:DD:EE:FF
```

tncd connects to the TNC directly via the BlueZ D-Bus Profile API — no
external tools like `rfcomm` needed.

## Existing config file

If you manage `/etc/tncd.ini` outside of Nix (e.g. via `environment.etc`),
point `configFile` at it to skip config generation:

```nix
services.tncd = {
  enable = true;
  configFile = /etc/tncd.ini;
};
```

## Multi-port configuration

Configure multiple TNCs and select the active one from your AGWPE client:

```nix
services.tncd = {
  enable = true;
  bluetooth.enable = true;
  settings = {
    server = {
      listen_host = "0.0.0.0";
      listen_port = 8000;
      callsign = "N0CALL";
    };
    "client.0" = {
      name = "TNC3 Mobilinkd (2m)";
      type = "bluetooth";
      bdaddr = "34:81:F4:3D:98:4B";
      ota_baudrate = 1200;
    };
    "client.1" = {
      name = "TS-2000 (HF)";
      type = "serial";
      device = "/dev/ttyUSB0";
      serial_baudrate = 57600;
      ota_baudrate = 1200;
    };
    "client.2" = {
      name = "Direwolf (testing)";
      type = "tcp";
      host = "127.0.0.1";
      port = 8001;
      ota_baudrate = 1200;
    };
    "kiss.1".tx_delay = 80;
  };
};
```

Ports are numbered starting at 0 and must be contiguous. Each port appears
in the AGWPE client's port selector with its configured `name`.

## KISS mode init string (serial TNCs)

For TNCs that need a command to enter KISS mode (e.g. Kantronics KPC-3):

```nix
services.tncd.settings."client.0" = {
  type = "serial";
  device = "/dev/ttyUSB0";
  serial_baudrate = 9600;
  ota_baudrate = 1200;
  init_string = "INT KISS\\r";
  init_delay = "1.0";
};
```

## Module options reference

| Option | Default | Description |
|--------|---------|-------------|
| `services.tncd.enable` | `false` | Enable the bridge |
| `services.tncd.package` | `pkgs.tncd` | Package to use |
| `services.tncd.configFile` | `null` | Use an existing INI file |
| `services.tncd.settings` | `{}` | Nix-generated INI config |
| `services.tncd.user` | `"tncd"` | Service user |
| `services.tncd.group` | `"tncd"` | Service group |
| `services.tncd.bluetooth.enable` | `false` | Add Bluetooth SPP deps and bluetooth group |
