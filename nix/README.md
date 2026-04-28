# Nix / NixOS support

Two files are provided:

| File | Purpose |
|------|---------|
| `overlay.nix` | Adds `pkgs.tncd` to nixpkgs |
| `module.nix` | NixOS module: `services.tncd.*` options + systemd services |

## Quick start (NixOS)

```nix
# configuration.nix
{ config, pkgs, ... }:
{
  nixpkgs.overlays = [ (import /path/to/tncd/nix/overlay.nix) ];
  imports = [ /path/to/tncd/nix/module.nix ];

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
        baudrate = 9600;
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
    };
    bluetooth = {
      enabled = true;
      bind_dev = "/dev/rfcomm0";
      bdaddr = "38:D2:00:01:52:8F";
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
  baudrate = 9600;
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

## Standalone nix-build

```bash
nix-build -I nixpkgs=/path/to/nixpkgs -f overlay.nix -A tncd
```
