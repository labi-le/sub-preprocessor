# NixOS module: add the sub-preprocessor Prometheus scrape job and Grafana
# dashboard provider.
#
# Assumes the host already runs Prometheus and Grafana. The app publishes its
# metrics as 127.0.0.1:9091 -> :9090 from docker-compose, the crawler its own
# counters as 127.0.0.1:9092 -> :9092 (CRAWL_HTTP), and the dashboard picks a
# Prometheus datasource through a template variable, so it needs no fixed
# datasource uid here.
{ ... }:
{
  services.prometheus.scrapeConfigs = [
    {
      # The job name is what the dashboard's Instance picker enumerates
      # (`label_values(stable_cycles_total, job)`), so a second deployment needs
      # its own job rather than a second target here, or it cannot be selected
      # at all.
      job_name = "sub-preprocessor";
      # Two targets, one job: the crawler is a sidecar of the same logical
      # service and renders a disjoint metric family (stable_crawl_*), so
      # nothing double-counts. It has to share the job because the dashboard's
      # Instance picker enumerates label_values(stable_cycles_total, job) and
      # the crawler's surface serves no stable_cycles_total of its own — under a
      # job of its own it could not be selected, and every crawler panel would
      # read No data.
      static_configs = [ { targets = [ "127.0.0.1:9091" "127.0.0.1:9092" ]; } ];
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
