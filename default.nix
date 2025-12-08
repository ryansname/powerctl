{ pkgs ? import <nixpkgs> { } }:

pkgs.buildGoModule {
  pname = "powerctl";
  version = "0.1.0";
  src = ./.;

  vendorHash = "sha256-njjld8dDjLeNLFCMQBp+SIE0kc1mRkL87aqCuS8CUOo=";
  subPackages = [ "src" ];

  nativeBuildInputs = [ pkgs.golangci-lint ];

  doCheck = true;

  checkPhase = ''
    runHook preCheck
    go test ./...
    HOME=$(mktemp -d) golangci-lint run
    runHook postCheck
  '';

  postInstall = ''
    mv $out/bin/src $out/bin/powerctl
  '';
}
