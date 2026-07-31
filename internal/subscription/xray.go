package subscription

import (
	"bytes"
	"encoding/json"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// A "subscription URL" comes in a second shape: panel software (Hiddify most
// visibly) serves an array of complete Xray client configs instead of a URI
// list. Everything downstream — Parse, classify, the geo pipeline, Merge,
// rewrite — speaks share links, so the outbounds are converted to share links
// here, at the same seam base64 is decoded, and no other package learns about
// JSON. Measured on one t.me/hiddifycode post: 6 of its 7 links served JSON
// holding 160 proxy outbounds, against 8 nodes in the single URI-list link.
//
// Only vless is converted. Those 160 outbounds were 158 vless and 2
// shadowsocks, one of the two carrying the literal address "sdfsdf" — a second
// protocol would be untested surface for ~1% of the payload.

// maxJSONOutbounds bounds the expansion. Normalize's output feeds
// preprocess.processBody, whose node ceiling is enforced on the EXPANDED body:
// truncating here is cheaper than expanding a hostile document and rejecting
// the whole source there.
const (
	maxJSONOutbounds = 50_000

	// queryHint sizes the share-link query map: the widest shape converted
	// (reality over ws) sets encryption, flow, type, security, sni, pbk, sid,
	// fp, path and host.
	queryHint = 10
)

// maybeXrayJSON converts body when it is an Xray config document. Normalize
// calls it BEFORE its "://" fast path on purpose: real configs carry DoH server
// URLs and an observatory probe destination, so 4 of the 5 JSON bodies measured
// contain "://" and would take that path and never be converted.
func maybeXrayJSON(body []byte) ([]byte, bool) {
	if len(body) == 0 || (body[0] != '[' && body[0] != '{') {
		return nil, false
	}

	return convertXrayJSON(body)
}

type xrayUser struct {
	ID         string `json:"id"`
	Encryption string `json:"encryption"`
	Flow       string `json:"flow"`
}

type xrayVNext struct {
	Address string     `json:"address"`
	Port    int        `json:"port"`
	Users   []xrayUser `json:"users"`
}

type xrayStream struct {
	Network  string `json:"network"`
	Security string `json:"security"`
	Reality  struct {
		ServerName  string `json:"serverName"`
		PublicKey   string `json:"publicKey"`
		ShortID     string `json:"shortId"`
		Fingerprint string `json:"fingerprint"`
	} `json:"realitySettings"`
	TLS struct {
		ServerName  string   `json:"serverName"`
		Fingerprint string   `json:"fingerprint"`
		ALPN        []string `json:"alpn"`
	} `json:"tlsSettings"`
	WS struct {
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
	} `json:"wsSettings"`
	GRPC struct {
		ServiceName string `json:"serviceName"`
	} `json:"grpcSettings"`
	XHTTP struct {
		Mode string `json:"mode"`
		Host string `json:"host"`
		Path string `json:"path"`
	} `json:"xhttpSettings"`
}

type xrayOutbound struct {
	Tag      string `json:"tag"`
	Protocol string `json:"protocol"`
	Settings struct {
		VNext []xrayVNext `json:"vnext"`
	} `json:"settings"`
	StreamSettings xrayStream `json:"streamSettings"`
}

type xrayConfig struct {
	Remarks   string         `json:"remarks"`
	Outbounds []xrayOutbound `json:"outbounds"`
}

// convertXrayJSON renders the vless outbounds of an Xray config document as
// newline-joined share links. The second return reports whether anything was
// converted; on false the caller keeps the original body, so a JSON document
// that is not an Xray config (or carries no vless outbound) still reaches the
// existing parsing path unchanged.
func convertXrayJSON(body []byte) ([]byte, bool) {
	configs, ok := decodeXrayConfigs(body)
	if !ok {
		return nil, false
	}

	var out bytes.Buffer
	written := 0
	for i := range configs {
		for j := range configs[i].Outbounds {
			if written >= maxJSONOutbounds {
				break
			}
			uri, uriOK := vlessShareLink(&configs[i].Outbounds[j], configs[i].Remarks)
			if !uriOK {
				continue
			}
			if written > 0 {
				out.WriteByte('\n')
			}
			out.WriteString(uri)
			written++
		}
	}
	if written == 0 {
		return nil, false
	}

	return out.Bytes(), true
}

// decodeXrayConfigs accepts both shapes seen in the wild: a single config
// object, and the array of configs a panel serves when it publishes several
// servers at once.
func decodeXrayConfigs(body []byte) ([]xrayConfig, bool) {
	switch body[0] {
	case '[':
		var many []xrayConfig
		if err := json.Unmarshal(body, &many); err != nil {
			return nil, false
		}

		return many, true
	case '{':
		var one xrayConfig
		if err := json.Unmarshal(body, &one); err != nil {
			return nil, false
		}

		return []xrayConfig{one}, true
	}

	return nil, false
}

// vlessShareLink builds the share link mihomo's convert.ConvertsV2Ray parses.
// Parameter names follow the Xray VLESS share-link standard, which is what
// mihomo's handleVShareLink reads; a name it does not read is dropped
// silently, so the mapping stays on the keys that survive into the proxy map.
func vlessShareLink(ob *xrayOutbound, remarks string) (string, bool) {
	if !strings.EqualFold(ob.Protocol, "vless") || len(ob.Settings.VNext) == 0 {
		return "", false
	}
	vnext := ob.Settings.VNext[0]
	if vnext.Address == "" || vnext.Port <= 0 || len(vnext.Users) == 0 || vnext.Users[0].ID == "" {
		return "", false
	}

	st := &ob.StreamSettings
	q := make(url.Values, queryHint)
	if enc := vnext.Users[0].Encryption; enc != "" {
		q.Set("encryption", enc)
	} else {
		q.Set("encryption", "none")
	}
	setNonEmpty(q, "flow", vnext.Users[0].Flow)

	// Xray renamed the plain-TCP transport to "raw"; mihomo's share-link
	// handler has no "raw" case, so the name would pass straight into
	// proxy["network"] and adapter.ParseProxy would refuse the node.
	network := strings.ToLower(st.Network)
	if network == "raw" || network == "" {
		network = "tcp"
	}
	q.Set("type", network)

	switch strings.ToLower(st.Security) {
	case "reality":
		q.Set("security", "reality")
		setNonEmpty(q, "sni", st.Reality.ServerName)
		setNonEmpty(q, "pbk", st.Reality.PublicKey)
		setNonEmpty(q, "sid", st.Reality.ShortID)
		setNonEmpty(q, "fp", st.Reality.Fingerprint)
	case "tls":
		q.Set("security", "tls")
		setNonEmpty(q, "sni", st.TLS.ServerName)
		setNonEmpty(q, "fp", st.TLS.Fingerprint)
		if len(st.TLS.ALPN) > 0 {
			q.Set("alpn", strings.Join(st.TLS.ALPN, ","))
		}
	}

	switch network {
	case "ws":
		setNonEmpty(q, "path", st.WS.Path)
		setNonEmpty(q, "host", headerValue(st.WS.Headers, "host"))
	case "grpc":
		setNonEmpty(q, "serviceName", st.GRPC.ServiceName)
	case "xhttp":
		setNonEmpty(q, "path", st.XHTTP.Path)
		setNonEmpty(q, "host", st.XHTTP.Host)
		setNonEmpty(q, "mode", st.XHTTP.Mode)
	}

	var b strings.Builder
	b.WriteString("vless://")
	b.WriteString(vnext.Users[0].ID)
	b.WriteByte('@')
	b.WriteString(hostForAuthority(vnext.Address))
	b.WriteByte(':')
	b.WriteString(strconv.Itoa(vnext.Port))
	b.WriteByte('?')
	b.WriteString(q.Encode())
	b.WriteByte('#')
	b.WriteString(url.PathEscape(outboundName(ob, remarks, vnext.Address)))

	return b.String(), true
}

// outboundName picks the display name. The config's own remarks win; the
// outbound tag is a fallback, but every Xray client config calls its proxy
// outbound "proxy", which would name every node in a multi-config document
// identically, so that one degrades to the server address.
func outboundName(ob *xrayOutbound, remarks, address string) string {
	if remarks != "" {
		return remarks
	}
	if ob.Tag != "" && ob.Tag != "proxy" {
		return ob.Tag
	}

	return address
}

func setNonEmpty(q url.Values, key, value string) {
	if value != "" {
		q.Set(key, value)
	}
}

// hostForAuthority brackets an IPv6 literal. Without it the authority reads
// "2001:db8::1:443" and splitHostPort treats the whole thing as a portless
// IPv6 host, so the port is lost and mihomo's url.Parse refuses the link.
func hostForAuthority(address string) string {
	if addr, err := netip.ParseAddr(address); err == nil && addr.Is6() && !addr.Is4In6() {
		return "[" + address + "]"
	}

	return address
}

// headerValue looks up an HTTP header case-insensitively. Xray configs are not
// consistent about the "Host" key's case, and a miss would dial the node with
// the wrong Host header instead of failing loudly.
func headerValue(headers map[string]string, key string) string {
	for k, v := range headers {
		if strings.EqualFold(k, key) {
			return v
		}
	}

	return ""
}
