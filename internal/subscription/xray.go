package subscription

import (
	"encoding/json"
	"net/netip"
	"net/url"
	"slices"
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
// vless and hysteria2 are converted; see appendOutboundShareLink for the measured
// protocol split that decides what is worth mapping.

// maxJSONOutbounds bounds the expansion. Normalize's output feeds
// preprocess.processBody, whose node ceiling is enforced on the EXPANDED body:
// truncating here is cheaper than expanding a hostile document and rejecting
// the whole source there.
const (
	maxJSONOutbounds = 50_000

	// queryHint sizes the share-link query array: the widest shape converted
	// (reality over ws) sets encryption, flow, type, security, sni, pbk, sid,
	// fp, path and host.
	queryHint = 10

	// hysteriaQueryHint sizes the hysteria2 query array: sni, alpn, insecure.
	hysteriaQueryHint = 3

	portBase = 10
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
		ServerName    string   `json:"serverName"`
		Fingerprint   string   `json:"fingerprint"`
		ALPN          []string `json:"alpn"`
		AllowInsecure bool     `json:"allowInsecure"`
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
	// Hysteria is not an Xray-core transport; panel builds carry it here, with
	// the credential where a vless config keeps users[].id.
	Hysteria struct {
		Version int    `json:"version"`
		Auth    string `json:"auth"`
	} `json:"hysteriaSettings"`
}

type xrayOutbound struct {
	Tag      string `json:"tag"`
	Protocol string `json:"protocol"`
	Settings struct {
		VNext []xrayVNext `json:"vnext"`
		// Hysteria outbounds put the endpoint directly on settings instead of
		// wrapping it in vnext/servers.
		Address string `json:"address"`
		Port    int    `json:"port"`
		Version int    `json:"version"`
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

	var out []byte
	written := 0
	for i := range configs {
		for j := range configs[i].Outbounds {
			if written >= maxJSONOutbounds {
				break
			}
			link, linkOK := appendOutboundShareLink(out, &configs[i].Outbounds[j], configs[i].Remarks)
			if !linkOK {
				continue
			}
			// The separator goes WITH the link and the last one is dropped
			// below, so a refused outbound can leave no blank line behind.
			out = link
			out = append(out, '\n')
			written++
			if written == 1 {
				// One Grow off the first link instead of a doubling chain. It
				// counts outbounds that convert to nothing (every config ships
				// a "direct" one) so it over-reserves rather than regrows;
				// reserving BEFORE the first link would charge a document that
				// converts nothing at all, which a sing-box config decodes as.
				out = slices.Grow(out, len(out)*(outboundCount(configs)-1))
			}
		}
	}
	if written == 0 {
		return nil, false
	}

	return out[:len(out)-1], true
}

func outboundCount(configs []xrayConfig) int {
	n := 0
	for i := range configs {
		n += len(configs[i].Outbounds)
	}

	return min(n, maxJSONOutbounds)
}

// appendOutboundShareLink dispatches on the outbound protocol. Measured across
// 25 JSON links from one channel's history: 344 vless, 35 hysteria, 6
// shadowsocks. Shadowsocks stays out at 1.6% -- and the sampled entry carried
// the literal address "sdfsdf".
//
// The link is appended into the caller's buffer rather than returned as a
// string: strings.Builder growth was 23% of this path's alloc_objects
// (2026-08-18), every bit of it for bytes then copied into the output again.
func appendOutboundShareLink(dst []byte, ob *xrayOutbound, remarks string) ([]byte, bool) {
	switch strings.ToLower(ob.Protocol) {
	case "vless":
		return appendVlessShareLink(dst, ob, remarks)
	case "hysteria", "hysteria2", "hy2":
		return appendHysteria2ShareLink(dst, ob, remarks)
	}

	return dst, false
}

// appendHysteria2ShareLink renders a hysteria2 outbound as the share link
// mihomo's converter reads: the credential is userinfo, everything else query.
//
// Version 2 ONLY. mihomo parses hysteria v1 under its own `hysteria://` scheme
// with a different parameter set, so rendering a v1 outbound as hysteria2 would
// produce a proxy adapter.ParseProxy accepts and the probe then reports as a
// dead node — a mapping bug wearing the costume of a bad source.
func appendHysteria2ShareLink(dst []byte, ob *xrayOutbound, remarks string) ([]byte, bool) {
	st := &ob.StreamSettings
	if !isHysteria2(ob) {
		return dst, false
	}
	if ob.Settings.Address == "" || ob.Settings.Port <= 0 || st.Hysteria.Auth == "" {
		return dst, false
	}

	var scratch [hysteriaQueryHint]queryPair
	q := setNonEmpty(queryList(scratch[:0]), "sni", st.TLS.ServerName)
	if len(st.TLS.ALPN) > 0 {
		q = q.set("alpn", strings.Join(st.TLS.ALPN, ","))
	}
	if st.TLS.AllowInsecure {
		q = q.set("insecure", "1")
	}

	dst = append(dst, "hysteria2://"...)
	dst = append(dst, url.User(st.Hysteria.Auth).String()...)
	dst = append(dst, '@')
	dst = appendHostForAuthority(dst, ob.Settings.Address)
	dst = append(dst, ':')
	dst = strconv.AppendInt(dst, int64(ob.Settings.Port), portBase)
	if len(q) > 0 {
		dst = append(dst, '?')
		dst = q.appendEncoded(dst)
	}
	dst = append(dst, '#')
	dst = appendPathEscape(dst, outboundName(ob, remarks, ob.Settings.Address))

	return dst, true
}

// isHysteria2 decides the protocol version. "hysteria2"/"hy2" name the version
// themselves, so demanding an explicit version field there would silently drop
// outbounds that simply omit it. Only the bare "hysteria" name is ambiguous,
// and that one has to say 2 — the version lives in either settings or
// hysteriaSettings depending on which panel wrote the config.
func isHysteria2(ob *xrayOutbound) bool {
	switch strings.ToLower(ob.Protocol) {
	case "hysteria2", "hy2":
		return true
	case "hysteria":
		return ob.StreamSettings.Hysteria.Version == 2 || ob.Settings.Version == 2
	}

	return false
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

// appendVlessShareLink builds the share link mihomo's convert.ConvertsV2Ray
// parses. Parameter names follow the Xray VLESS share-link standard, which is
// what mihomo's handleVShareLink reads; a name it does not read is dropped
// silently, so the mapping stays on the keys that survive into the proxy map.
func appendVlessShareLink(dst []byte, ob *xrayOutbound, remarks string) ([]byte, bool) {
	if !strings.EqualFold(ob.Protocol, "vless") || len(ob.Settings.VNext) == 0 {
		return dst, false
	}
	vnext := &ob.Settings.VNext[0]
	if vnext.Address == "" || vnext.Port <= 0 || len(vnext.Users) == 0 || vnext.Users[0].ID == "" {
		return dst, false
	}

	st := &ob.StreamSettings
	var scratch [queryHint]queryPair
	q := queryList(scratch[:0])
	if enc := vnext.Users[0].Encryption; enc != "" {
		q = q.set("encryption", enc)
	} else {
		q = q.set("encryption", "none")
	}
	q = setNonEmpty(q, "flow", vnext.Users[0].Flow)

	// Xray renamed the plain-TCP transport to "raw"; mihomo's share-link
	// handler has no "raw" case, so the name would pass straight into
	// proxy["network"] and adapter.ParseProxy would refuse the node.
	network := strings.ToLower(st.Network)
	if network == "raw" || network == "" {
		network = "tcp"
	}
	q = q.set("type", network)

	switch strings.ToLower(st.Security) {
	case "reality":
		q = q.set("security", "reality")
		q = setNonEmpty(q, "sni", st.Reality.ServerName)
		q = setNonEmpty(q, "pbk", st.Reality.PublicKey)
		q = setNonEmpty(q, "sid", st.Reality.ShortID)
		q = setNonEmpty(q, "fp", st.Reality.Fingerprint)
	case "tls":
		q = q.set("security", "tls")
		q = setNonEmpty(q, "sni", st.TLS.ServerName)
		q = setNonEmpty(q, "fp", st.TLS.Fingerprint)
		if len(st.TLS.ALPN) > 0 {
			q = q.set("alpn", strings.Join(st.TLS.ALPN, ","))
		}
	}

	switch network {
	case "ws":
		q = setNonEmpty(q, "path", st.WS.Path)
		q = setNonEmpty(q, "host", headerValue(st.WS.Headers, "host"))
	case "grpc":
		q = setNonEmpty(q, "serviceName", st.GRPC.ServiceName)
	case "xhttp":
		q = setNonEmpty(q, "path", st.XHTTP.Path)
		q = setNonEmpty(q, "host", st.XHTTP.Host)
		q = setNonEmpty(q, "mode", st.XHTTP.Mode)
	}

	dst = append(dst, "vless://"...)
	dst = append(dst, vnext.Users[0].ID...)
	dst = append(dst, '@')
	dst = appendHostForAuthority(dst, vnext.Address)
	dst = append(dst, ':')
	dst = strconv.AppendInt(dst, int64(vnext.Port), portBase)
	dst = append(dst, '?')
	dst = q.appendEncoded(dst)
	dst = append(dst, '#')
	dst = appendPathEscape(dst, outboundName(ob, remarks, vnext.Address))

	return dst, true
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

func setNonEmpty(q queryList, key, value string) queryList {
	if value == "" {
		return q
	}

	return q.set(key, value)
}

// appendHostForAuthority brackets an IPv6 literal. Without it the authority
// reads "2001:db8::1:443" and splitHostPort treats the whole thing as a
// portless IPv6 host, so the port is lost and mihomo's url.Parse refuses the
// link.
func appendHostForAuthority(dst []byte, address string) []byte {
	if !ipv6Literal(address) {
		return append(dst, address...)
	}
	dst = append(dst, '[')
	dst = append(dst, address...)

	return append(dst, ']')
}

// ipv6Literal gates the parse on a ':', which no textual IPv6 address is
// written without: netip.ParseAddr boxes its error on the heap, so every
// hostname address paid one allocation per link to be told it is not an address
// (3% of this path's alloc_objects, 2026-08-18).
func ipv6Literal(address string) bool {
	if strings.IndexByte(address, ':') < 0 {
		return false
	}
	addr, err := netip.ParseAddr(address)

	return err == nil && addr.Is6() && !addr.Is4In6()
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
