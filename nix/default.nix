{ lib
, buildGoModule
}:

buildGoModule rec {
  pname = "tncd";
  version = "1.98-Beta";

  src = lib.cleanSource ../.;

  # Pin the module dependency hash. Update when go.mod/go.sum change:
  #   set to lib.fakeHash, build, and copy the "got:" hash from the error.
  vendorHash = "sha256-FFRXOD48HO+2C3m95wkFYpAIXmHytpXFSxl5TYbnjR8=";

  env.CGO_ENABLED = "0";

  # Build only the daemon; skip helper/example mains if any are added later.
  subPackages = [ "cmd/tncd" ];

  ldflags = [
    "-s"
    "-w"
    "-X github.com/ben-kuhn/tncd/v2/internal/version.Version=${version}"
  ];

  postInstall = ''
    install -Dm644 tncd.ini $out/share/tncd/tncd.ini.example
  '';

  meta = with lib; {
    description = "AGWPE-to-KISS Translation Bridge";
    longDescription = ''
      A bridge that allows AGWPE-client applications to communicate with KISS
      TNCs. Supports both serial and TCP KISS connections, and full AX.25
      connected mode. This is the Go port (tncd 2.0 line).
    '';
    homepage = "https://tncd.dev";
    license = licenses.gpl3Only;
    maintainers = [ ];
    platforms = platforms.linux;
    mainProgram = "tncd";
  };
}
