# NixOS module: add the sub-preprocessor Prometheus scrape job and Grafana
# dashboard provider.
#
# Assumes the host already runs Prometheus and Grafana. The app publishes its
# metrics as 127.0.0.1:9091 -> :9090 from docker-compose, and the dashboard
# picks a Prometheus datasource through a template variable, so it needs no
# fixed datasource uid here.
{ ... }:
{
  services.prometheus.scrapeConfigs = [
    {
      # The job name is what the dashboard's Instance picker enumerates
      # (`label_values(stable_cycles_total, job)`), so a second deployment needs
      # its own job rather than a second target here, or it cannot be selected
      # at all.
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
