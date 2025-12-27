{ pkgs ? import <nixpkgs> { } }:

pkgs.buildGoModule {
  pname = "powerctl";
  version = "0.1.0";
  src = ./.;

  vendorHash = "sha256-vl8ELHtVdMQrM5Ed56lLQr2u0AWZuXZUaL7VcPLxSvc=";
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
