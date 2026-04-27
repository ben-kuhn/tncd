{ lib
, python3
}:

python3.pkgs.buildPythonApplication rec {
  pname = "agwkiss";
  version = "1.0.0";

  src = lib.cleanSource ../.;

  format = "other";

  disabled = python3.pkgs.pythonOlder "3.8";

  dependencies = with python3.pkgs; [
    pyserial
    # kiss3 and pyham-ax25 must be provided as custom derivations
    # if not yet available in the nixpkgs channel being used.
  ];

  installPhase = ''
    install -Dm755 agwkiss.py $out/bin/agwkiss
  '';

  meta = with lib; {
    description = "AGWPE-to-KISS Translation Bridge";
    longDescription = ''
      A bridge that allows AGWPE-client applications to communicate with KISS TNCs.
      Supports both serial and TCP KISS connections.
    '';
    homepage = "https://github.com/agwkit/agwkiss";
    license = lib.licenses.gpl3;
    maintainers = [ ];
    platforms = lib.platforms.linux;
  };
}
