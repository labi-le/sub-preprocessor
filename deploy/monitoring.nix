# NixOS module: scrape the sub-preprocessor's loopback /metrics endpoint and
# provision its Grafana dashboard. It is co-located with the app so the
# dashboard stays in lockstep with the metric names it renders; the NixOS repo
# only pulls this in as a flake input.
#
# Consume from a host that runs Prometheus + Grafana (both on loopback):
#   inputs.sub-preprocessor.url = "git+ssh://git@github.com/labi-le/sub-preprocessor";
#   imports = [ inputs.sub-preprocessor.nixosModules.monitoring ];
#
# Assumes docker-compose publishes the service's metrics port loopback-only:
# 9091 for sub-preprocessor (see ../docker-compose.yaml). The dashboard selects
# the Prometheus datasource via a template variable, so it needs no fixed
# datasource uid.
{ ... }:
{
  # The job name is what the dashboard's Instance picker enumerates
  # (`label_values(stable_cycles_total, job)`), so a second deployment needs its
  # own job rather than a second target here, or it cannot be selected at all.
  services.prometheus.scrapeConfigs = [
    {
      job_name = "sub-preprocessor";
      static_configs = [ { targets = [ "127.0.0.1:9091" ]; } ];
    }
  ];

  services.grafana.provision.dashboards.settings = {
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
}
