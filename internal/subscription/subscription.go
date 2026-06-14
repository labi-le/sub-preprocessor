package subscription

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"

	"domains.lst/sub-preprocessor/internal/fetch"
)

const maxSubscriptionSize = 10 << 20

type Node struct {
	Raw    string
	URL    *url.URL
	Name   string
	Server string
	Port   string
}

func Load(ctx context.Context, rawURL string) ([]Node, error) {
	body, err := fetch.BytesWithType(ctx, rawURL, maxSubscriptionSize, fetch.FileTypeRaw)
	if err != nil {
		return nil, err
	}
	return Parse(normalize(body))
}

func Parse(body []byte) ([]Node, error) {
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 1024), 1024*1024)

	var nodes []Node
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		u, err := url.Parse(line)
		if err != nil || strings.ToLower(u.Scheme) != "vless" {
			continue
		}

		server := u.Hostname()
		if server == "" {
			continue
		}

		port := u.Port()
		if port == "" {
			port = "443"
		}

		name := strings.TrimSpace(u.Fragment)
		if name == "" {
			name = server
		}

		nodes = append(nodes, Node{Raw: line, URL: u, Name: name, Server: server, Port: port})
	}

	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, errors.New("no vless URI nodes found")
	}

	return nodes, nil
}

func normalize(body []byte) []byte {
	body = bytes.TrimSpace(body)
	if bytes.Contains(body, []byte("://")) {
		return body
	}

	compact := string(body)
	compact = strings.ReplaceAll(compact, "\n", "")
	compact = strings.ReplaceAll(compact, "\r", "")
	compact = strings.ReplaceAll(compact, "\t", "")
	compact = strings.ReplaceAll(compact, " ", "")

	if decoded, err := base64.StdEncoding.DecodeString(compact); err == nil && bytes.Contains(decoded, []byte("://")) {
		return bytes.TrimSpace(decoded)
	}
	if decoded, err := base64.RawStdEncoding.DecodeString(compact); err == nil && bytes.Contains(decoded, []byte("://")) {
		return bytes.TrimSpace(decoded)
	}

	return body
}
