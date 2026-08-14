{ pkgs ? import <nixpkgs> {} }:

pkgs.mkShell {
  packages = with pkgs; [
    go
    gcc
    gopls
    gnumake
    golangci-lint
    goreleaser
  ];

  shellHook = ''
    export CGO_ENABLED=0

    echo "sub-preprocessor dev shell"
    echo "  run : make run"
    echo "  test: make test"
    echo "  fmt : make fmt"
    echo "  race: make race"
    echo "  bench: make bench"
    echo "  lint: make lint"
    echo "  default: make"
    echo "  curl: curl \"http://127.0.0.1:8080/?subscription_url=https://raw.githubusercontent.com/flaafix/AetrisVPN-black-list/refs/heads/main/configs.txt&countries=FI,EE,LV,LT,SE,PL,DE,NL\""
  '';
}
