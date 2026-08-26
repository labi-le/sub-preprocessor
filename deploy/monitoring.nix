# NixOS module: scrape the sub-preprocessor's loopback /metrics endpoint and
# provision its Grafana dashboard beside the metric names it renders.
#
# Service enablement, listen addresses and retention are mkDefault so a host
# that already runs Prometheus/Grafana keeps its own values.
#
# Host-side Grafana still MUST provide settings.security.secret_key; nixpkgs
# 26.11 asserts on it. The admin password stays host policy rather than this
# module's.
#
# Assumes docker-compose publishes the service's metrics port loopback-only as
# 127.0.0.1:9091 -> :9090. The dashboard picks a Prometheus datasource through
# a template variable, so the datasource needs no fixed uid.
{ lib, ... }:
{
  services.prometheus = {
    enable = lib.mkDefault true;
    listenAddress = lib.mkDefault "127.0.0.1";
    # port stays the 9090 default; the app container publishes 9091 to the host
    retentionTime = lib.mkDefault "30d";
    # The job name is what the dashboard's Instance picker enumerates
    # (`label_values(stable_cycles_total, job)`), so a second deployment needs
    # its own job rather than a second target here, or it cannot be selected
    # at all.
    scrapeConfigs = [
      {
        job_name = "sub-preprocessor";
        static_configs = [ { targets = [ "127.0.0.1:9091" ]; } ];
      }
    ];
  };

  services.grafana = {
    enable = lib.mkDefault true;
    settings.server = {
      http_addr = lib.mkDefault "127.0.0.1";
      http_port = lib.mkDefault 3000;
    };
    provision = {
      # Vestigial in nixpkgs 26.11 (declared at grafana.nix:1330 but consumed
      # nowhere; the provisioning dir is symlinked unconditionally), kept so
      # the provisioning below still takes effect on older nixpkgs where this
      # option gated it.
      enable = lib.mkDefault true;
      datasources.settings = {
        apiVersion = 1;
        datasources = [
          {
            name = "Prometheus";
            type = "prometheus";
            access = "proxy";
            url = "http://127.0.0.1:9090";
            isDefault = true;
          }
        ];
      };
      dashboards.settings = {
        apiVersion = 1;
        providers = [
          {
            name = "sub-preprocessor";
            type = "file";
            disableDeletion = true;
            options = {
              path = ./grafana;
              foldersFromFilesStructure = false;
            };
          }
        ];
      };
    };
  };
}
