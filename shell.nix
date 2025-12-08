{ pkgs ? import <nixpkgs> { config.allowUnfree = true; } }:

pkgs.mkShell {
  buildInputs = with pkgs; [
    go
    gopls
    golangci-lint
    claude-code
    claude-monitor
  ];

  shellHook = ''
    echo "🚀 Development environment loaded!"
    echo ""
    echo "Available tools:"
    echo "  • go                  - Go programming language ($(go version | cut -d' ' -f3))"
    echo "  • gopls               - Go language server"
    echo "  • golangci-lint       - Go code linter aggregator"
    echo "  • claude              - Claude Code CLI tool"
    echo "  • claude-code-monitor - Claude API usage monitor"
  '';
}
