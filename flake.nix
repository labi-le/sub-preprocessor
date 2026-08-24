{
  description = "Sub-preprocessor flake";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
    }:
    let
      pname = "sub-preprocessor";
      supportedSystems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
    in
    flake-utils.lib.eachSystem supportedSystems (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
      in
      {
        packages.default = pkgs.buildGoModule {
          inherit pname;
          # no release tags exist to version against; the build tracks the pinned rev
          version = self.shortRev or self.dirtyShortRev;
          src = self;
          subPackages = [ "." ];
          vendorHash = "sha256-acgA9ktV7Pvc4fZcBxGLMeS29NgCnhRLG+OyCfNiPmY=";

          env.CGO_ENABLED = "0";

          meta = {
            description = "Sub-preprocessor";
            homepage = "https://github.com/labi-le/sub-preprocessor";
            license = pkgs.lib.licenses.mit;
            platforms = supportedSystems;
          };
        };
      }
    )
    // {
      nixosModules.default =
        {
          config,
          lib,
          pkgs,
          ...
        }:
        let
          cfg = config.services.sub-preprocessor;
          defaultPackage = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
        in
        {
          options.services.sub-preprocessor = with lib; {
            enable = mkEnableOption "Sub-preprocessor service";

            package = mkOption {
              type = types.package;
              default = defaultPackage;
              description = "The sub-preprocessor package to use";
            };
          };

          config = lib.mkIf cfg.enable {
            environment.systemPackages = [ cfg.package ];

            systemd.services.sub-preprocessor = {
              description = "Sub-preprocessor Service";
              after = [ "network.target" ];
              wantedBy = [ "multi-user.target" ];

              serviceConfig = {
                Type = "simple";
                Restart = "on-failure";
                RestartSec = "10";
                ExecStart = "${cfg.package}/bin/sub-preprocessor";
                WorkingDirectory = "/var/lib/sub-preprocessor";
                StateDirectory = "sub-preprocessor";
              };
            };
          };
        };
      # Monitoring wiring for a host that runs Prometheus + Grafana; imported
      # by the nixos repo as inputs.sub-preprocessor.nixosModules.monitoring.
      nixosModules.monitoring = import ./deploy/monitoring.nix;
    };
}
