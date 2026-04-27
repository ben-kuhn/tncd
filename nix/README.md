# Nix Overlay for AGWKISS

This overlay allows you to use agwkiss in NixOS without adding it to the main nixpkgs repository.

## Quick Start

### Option 1: Import directly in configuration.nix

```nix
# In your configuration.nix
{ config, pkgs, ... }:

{
  imports = [
    /path/to/agwkiss/nix/overlay.nix
  ];

  environment.systemPackages = [ pkgs.agwkiss ];
  
  # Or use it in a container
  virtualisation.oci-containers.containers.aprs = {
    image = "agwkiss";
    # ...
  };
}
```

### Option 2: Using flakes (modern)

```nix
# In flake.nix
{
  inputs.agwkiss.url = "github:yourusername/agwkiss";
  inputs.agwkiss.overlays.default = Agwkiss: prev: {
    agwkiss = prev.python3Packages.buildPythonApplication {
      pname = "agwkiss";
      version = "1.0.0";
      src = Agwkiss;
      dependencies = with prev.python3Packages; [
        pyserial
        ax253
      ];
    };
  };
}
```

### Option 3: Standalone nix-build

```bash
# Build the package
nix-build -I nixpkgs=/path/to/nixpkgs -f overlay.nix -A agwkiss

# Create a GC root
nix-build -I nixpkgs=/path/to/nixpkgs -f overlay.nix -A agwkiss --out-link /var/lib/agwkiss
```

## Systemd Service on NixOS

```nix
# In configuration.nix
{ config, pkgs, ... }:

{
  systemd.services.agwkiss = {
    description = "AGWPE-to-KISS Bridge";
    wantedBy = [ "multi-user.target" ];
    serviceConfig = {
      ExecStart = "${pkgs.agwkiss}/bin/agwkiss.py -c /etc/agwkiss.ini";
      User = "aprs";
      Group = "aprs";
      Restart = "on-failure";
      RestartSec = 5;
    };
    wantedBy = [ "multi-user.target" ];
  };
}
```

## Configuration File

Create `/etc/agwkiss.ini`:

```ini
[server]
listen_host = 0.0.0.0
listen_port = 8000

[client]
type = serial
device = /dev/ttyUSB0
baudrate = 9600
```

## Updating the Overlay

When agwkiss is updated, the overlay will automatically pick up the changes if you point to the updated source directory.

For version pins, modify the src in the overlay:

```nix
src = final.fetchFromGitHub {
  owner = "yourusername";
  repo = "agwkiss";
  rev = "v1.0.0";
  sha256 = "0000000000000000000000000000000000000000000000000000=";
};
```

## Testing

```bash
# Test building the package
nix-build -I nixpkgs=/path/to/nixpkgs -f overlay.nix -A agwkiss

# Test the shell with dependencies
nix-shell -I nixpkgs=/path/to/nixpkgs -f overlay.nix -A agwkiss.drv
```