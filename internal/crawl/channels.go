package crawl

import (
	"os"

	"gopkg.in/yaml.v3"
)

// channelsFile is the seed-channel config, analogous to config.yaml/private.yaml.
// Entries may be bare slugs, @handles, or t.me URLs (normalized on use).
//
//	channels:
//	  - o00000000i
//	  - "@rap_ex"
//	  - https://t.me/remiuc
type channelsFile struct {
	Channels []string `yaml:"channels"`
}

// loadChannels reads the seed-channel list from a YAML file. It is best-effort:
// a missing path, unreadable file, or malformed YAML yields no channels (the
// crawler falls back to CRAWL_CHANNELS and remembered productive channels)
// rather than failing, so a bad edit never takes the crawler down. Read every
// cycle, it gives the channel list hot-reload without a container restart.
func loadChannels(path string) []string {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cf channelsFile
	if unmarshalErr := yaml.Unmarshal(b, &cf); unmarshalErr != nil {
		return nil
	}
	return cf.Channels
}
