# NixOS module: scrape the sub-preprocessor's loopback /metrics endpoint and
# provision its Grafana dashboard. It is co-located with the app so the
# dashboard stays in lockstep with the metric names it renders; the NixOS repo
# only pulls this in as a flake input.
#
# Consume from a host that runs Prometheus + Grafana (both on loopback):
#   inputs.sub-preprocessor.url = "git+ssh://git@github.com/labi-le/sub-preprocessor";
#   imports = [ inputs.sub-preprocessor.nixosModules.monitoring ];
#
# Assumes docker-compose publishes each instance's metrics port loopback-only:
# 9091 for sub-preprocessor, 9092 for sub-preprocessor-vassago (see
# ../docker-compose.yaml). The dashboard selects the Prometheus datasource via
# a template variable, so it needs no fixed datasource uid.
{ ... }:
{
  # A second job rather than a second target in the first: the dashboard's
  # Instance picker is `label_values(stable_cycles_total, job)`, so the two
  # deployments have to carry different job names to be selectable at all.
  services.prometheus.scrapeConfigs = [
    {
      job_name = "sub-preprocessor";
      static_configs = [ { targets = [ "127.0.0.1:9091" ]; } ];
    }
    {
      job_name = "sub-preprocessor-vassago";
      static_configs = [ { targets = [ "127.0.0.1:9092" ]; } ];
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
