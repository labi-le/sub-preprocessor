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
      version = "1.0.0";
      pname = "sub-preprocessor";
      supportedSystems = [
        "x86_64-linux"
        "aarch64-linux"
      ];

      systemConfigs = {
        x86_64-linux = {
          arch = "linux_amd64";
          hash = ""; # x86_64-linux
        };
        aarch64-linux = {
          arch = "linux_arm64";
          hash = ""; # aarch64-linux
        };
      };
    in
    flake-utils.lib.eachSystem supportedSystems (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
        config = systemConfigs.${system};
      in
      {
        packages.default = pkgs.stdenv.mkDerivation {
          inherit pname version;

          src = pkgs.fetchurl {
            url = "https://github.com/labi-le/sub-preprocessor/releases/download/v${version}/${pname}_${version}_${config.arch}";
            hash = config.hash;
          };

          dontUnpack = true;

          installPhase = ''
            mkdir -p $out/bin
            cp $src $out/bin/${pname}
            chmod +x $out/bin/${pname}
          '';

          meta = with pkgs.lib; {
            description = "Sub-preprocessor";
            homepage = "https://github.com/labi-le/sub-preprocessor";
            license = licenses.mit;
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
    };
}
