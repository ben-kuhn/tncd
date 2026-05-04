# Nix / NixOS support

tncd is packaged in [nix-ham-packages](https://github.com/ben-kuhn/nix-ham-packages),
which provides the `tncd` package and all its dependencies (`kiss3`, `pyham-ax25`, etc.)
as a nixpkgs overlay.

The NixOS service module (`module.nix`) in this repository provides `services.tncd.*`
options and systemd services.

## Quick start (NixOS)

```nix
# configuration.nix
{ config, pkgs, ... }:
{
  nixpkgs.overlays = [
    (import (builtins.fetchTarball
      "https://github.com/ben-kuhn/nix-ham-packages/archive/main.tar.gz"
    ))
  ];

  imports = [
    ((builtins.fetchTarball
      "https://github.com/ben-kuhn/tncd/archive/main.tar.gz"
    ) + "/nix/module.nix")
  ];

  services.tncd = {
    enable = true;
    settings = {
      server = {
        listen_host = "0.0.0.0";
        listen_port = 8000;
        callsign = "N0CALL";
      };
      client = {
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

Enable the rfcomm manager alongside the main bridge:

```nix
services.tncd = {
  enable = true;
  bluetooth.enable = true;   # also starts tncd-rfcomm.service
  settings = {
    server = {
      listen_host = "0.0.0.0";
      listen_port = 8000;
      callsign = "N0CALL";
    };
    client = {
      type = "serial";
      device = "/dev/rfcomm0";
      serial_baudrate = 9600;
      ota_baudrate = 1200;
    };
    bluetooth = {
      enabled = true;
      bind_dev = "/dev/rfcomm0";
      bdaddr = "AA:BB:CC:DD:EE:FF";
      channel = 1;
      mode = "watch";
      retry_delay = 5;
    };
  };
};
```

`tncd-rfcomm.service` runs as root (required for `rfcomm` commands) and will
start before `tncd.service`.

## Existing config file

If you manage `/etc/tncd.ini` outside of Nix (e.g. via `environment.etc`),
point `configFile` at it to skip config generation:

```nix
services.tncd = {
  enable = true;
  configFile = /etc/tncd.ini;
};
```

## KISS mode init string (serial TNCs)

For TNCs that need a command to enter KISS mode (e.g. Kantronics KPC-3):

```nix
services.tncd.settings.client = {
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
| `services.tncd.bluetooth.enable` | `false` | Also run rfcomm manager |
