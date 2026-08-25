package crawl

import (
	"os"

	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"
)

// channelsFile is the seed-channel config, analogous to config.yaml/private.yaml.
// Entries may be bare slugs, @handles, or t.me URLs (normalized on use). A
// trailing "/<topic>" names a forum topic inside a group and is kept: such a
// chat has no t.me/s/ preview to scrape at all, so the topic id is the only way
// to reach its messages (see topicQuery).
//
//	channels:
//	  - o00000000i
//	  - "@rap_ex"
//	  - https://t.me/remiuc
//	  - somegroup/1310
type channelsFile struct {
	Channels []string `yaml:"channels"`
}

// loadChannels reads the seed-channel list from a YAML file. It is best-effort:
// a missing path, unreadable file, or malformed YAML yields nothing (the crawler
// falls back to CRAWL_CHANNELS and remembered productive channels) rather than
// failing, so a bad edit never takes the crawler down — but read and unmarshal
// failures are logged so a broken edit is visible. Read every cycle, it gives
// the list hot-reload without a container restart.
func loadChannels(path string, logger zerolog.Logger) channelsFile {
	if path == "" {
		return channelsFile{}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn().Err(err).Str("path", path).Msg("read channels file failed")
		}
		return channelsFile{}
	}
	var cf channelsFile
	if unmarshalErr := yaml.Unmarshal(b, &cf); unmarshalErr != nil {
		logger.Warn().Err(unmarshalErr).Str("path", path).Msg("unmarshal channels file failed")
		return channelsFile{}
	}
	return cf
}
