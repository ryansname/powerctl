{ pkgs ? import <nixpkgs> { } }:

pkgs.buildGoModule {
  pname = "powerctl";
  version = "0.1.0";
  src = ./.;

  vendorHash = "sha256-Y7gaLGUnawahn71b4POINrF4zn472pkJD5qxKJ21UoA=";
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
