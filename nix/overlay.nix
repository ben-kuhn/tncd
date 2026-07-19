# Overlay providing the tncd package.
#
# Usage in configuration.nix:
#   nixpkgs.overlays = [ (import /path/to/tncd/nix/overlay.nix) ];
#
# Then import the NixOS module separately:
#   imports = [ /path/to/tncd/nix/module.nix ];
final: prev:
{
  tncd = final.callPackage ./default.nix { };
}
