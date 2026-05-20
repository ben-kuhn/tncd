# Overlay providing the tncd package.
#
# Usage in configuration.nix:
#   nixpkgs.overlays = [ (import /path/to/tncd/nix/overlay.nix) ];
#
# Then import the NixOS module separately:
#   imports = [ /path/to/tncd/nix/module.nix ];
final: prev:
{
  tncd = final.python3.pkgs.buildPythonApplication {
    pname = "tncd";
    version = "0.11.2-BETA";
    src = final.lib.cleanSource ../.;
    format = "other";
    disabled = final.python3.pkgs.pythonOlder "3.8";
    dependencies = with final.python3.pkgs; [
      pyserial
      # kiss3 and pyham-ax25 must be provided as custom derivations
      # if not yet available in the nixpkgs channel being used.
    ];
    installPhase = ''
      install -Dm755 tncd.py      $out/bin/tncd
      install -Dm644 tncd.ini     $out/share/tncd/tncd.ini.example
    '';
    meta = with final.lib; {
      description = "AGWPE-to-KISS Translation Bridge";
      longDescription = ''
        A bridge that allows AGWPE-client applications to communicate with KISS TNCs.
        Supports both serial and TCP KISS connections, and full AX.25 connected mode.
      '';
      homepage = "https://tncd.dev";
      license = final.lib.licenses.gpl3;
      platforms = final.lib.platforms.linux;
    };
  };
}
