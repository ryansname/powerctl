{ pkgs ? import <nixpkgs> { config.allowUnfree = true; } }:

pkgs.mkShell {
  buildInputs = with pkgs; [
    go
    gopls
    golangci-lint
  ];

  shellHook = ''
    echo "🚀 Development environment loaded!"
    echo ""
    echo "Available tools:"
    echo "  • go            - Go programming language ($(go version | cut -d' ' -f3))"
    echo "  • gopls         - Go language server"
    echo "  • golangci-lint - Go code linter aggregator"
  '';
}
