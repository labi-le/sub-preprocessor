package subscription

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"
)

// nameCorpus is what a display name can carry that json.Marshal treats
// differently: the bytes it escapes, the two runes it escapes, valid non-ASCII
// and invalid UTF-8.
var nameCorpus = []string{
	"", "node", "[GEO:FI][IP:192.0.2.1] mifa-001", "a b\tc",
	`quote " inside`, `back \ slash`, "html <b>&</b>", "100%",
	"Ünïtéd ÿÿÿ", "raw emoji 🇩🇪", "line\u2028sep", "para\u2029sep",
	"\xff invalid", "trunc \xf0\x9f", "nul \x00 byte", "del \x7f byte",
}

// TestAppendJSONStringMatchesMarshal is the equivalence proof for the escape-free
// path RewriteVmessName splices with: a name it emitted where json.Marshal would
// have escaped is a payload mihomo reads as a different name, or not at all.
func TestAppendJSONStringMatchesMarshal(t *testing.T) {
	t.Parallel()

	const singleBytes = 256
	cases := make([]string, 0, len(nameCorpus)+singleBytes)
	cases = append(cases, nameCorpus...)
	for b := range singleBytes {
		cases = append(cases, "x"+string([]byte{byte(b)}))
	}

	for _, s := range cases {
		want, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("json.Marshal(%q): %v", s, err)
		}
		got, ok := appendJSONString(nil, s)
		if !ok {
			t.Errorf("appendJSONString(%q) refused a string json.Marshal encoded", s)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("appendJSONString(%q) = %s, json.Marshal = %s", s, got, want)
		}
	}
}

// TestAppendJSONStringKeepsPrefix: the splice appends into a document being
// rebuilt, so a destination it overwrote would lose the fields before "ps".
func TestAppendJSONStringKeepsPrefix(t *testing.T) {
	t.Parallel()

	const prefix = `{"add":"192.0.2.1","ps":`
	got, ok := appendJSONString([]byte(prefix), "name")
	if !ok || string(got) != prefix+`"name"` {
		t.Fatalf("appendJSONString overwrote its destination: %q (ok=%v)", got, ok)
	}
}

// vmessAgreementCorpus is every payload shape that could make the field walker
// and the map decode disagree: nesting that repeats a wanted key, repeated keys
// at the top level, escapes in keys and values, non-string scalars, documents
// that are not objects, and documents encoding/json refuses outright.
var vmessAgreementCorpus = []string{
	`{"add":"1.2.3.4","port":443,"ps":"node"}`,
	`{"add":"1.2.3.4","port":"443","ps":"node"}`,
	`{"ps":"node","port":8080,"add":"host.example"}`,
	`{}`,
	`null`,
	`[]`,
	`[{"add":"1.2.3.4"}]`,
	`"a string"`,
	`123`,
	`true`,
	`{"add":"first","add":"last"}`,
	`{"ps":"first","ps":"","add":"x"}`,
	`{"\u0061dd":"escaped key","port":1}`,
	`{"a\u0064d":"escaped key mid","port":1}`,
	`{"ADD":"upper","Ps":"mixed","Port":1}`,
	`{"x":{"add":"nested","ps":"nested"},"add":"real","ps":"realname"}`,
	`{"add":"real","x":{"add":"nested"}}`,
	`{"x":["add",{"ps":"nested"},[{"port":9}]],"ps":"real","port":1}`,
	`{"add":"quote \" inside","ps":"back \\ slash"}`,
	`{"add":"brace } inside","ps":"comma , inside"}`,
	"{ \"add\" \t: \r\n \"spaced\" \n, \"port\" : 8080 }",
	`{"add":"x","port":null,"ps":true}`,
	`{"add":123,"port":1e3,"ps":-4}`,
	`{"add":"x","port":0.5}`,
	`{"ps":"emoji \ud83c\uddfa\ud83c\uddf8","add":"x"}`,
	`{"ps":"raw emoji 🇩🇪","add":"x"}`,
	`{"ps":"nul \u0000 escape","add":"x"}`,
	"{\"ps\":\"invalid utf8 \xff byte\",\"add\":\"x\"}",
	"{\"a\xffd\":\"invalid utf8 key\",\"add\":\"x\"}",
	`{"add":"x","deep":{"a":{"b":[1,2,{"add":"no","ps":"no"}]}},"port":7}`,
	`{"add":"","port":"","ps":""}`,
	`{"add":"x",}`,
	`{'add':'x'}`,
	`{"add":"x"`,
	`{"add":x}`,
	``,
	`   `,
	"\n\t{\"add\":\"x\",\"ps\":\"y\"}\n\t",
	`{"add":"x"} trailing garbage`,
	`{"add":"x","port":00}`,
	`{"v":"2","ps":"real","add":"1.2.3.4","port":"443","id":"uuid","aid":"0","net":"ws","type":"none","host":"h","path":"/p","tls":"tls","sni":"s","alpn":"h2","fp":"chrome","scy":"auto"}`,
}

// TestVmessFieldsAgreeWithMapDecode is the equivalence proof for the walker that
// replaced the map decode on the parse path and now gates the rewrite path too,
// so the two must keep answering identically for every document: a walker that
// accepted one more shape would keep nodes mihomo drops, and one that accepted
// fewer would drop nodes it converts.
func TestVmessFieldsAgreeWithMapDecode(t *testing.T) {
	t.Parallel()

	for _, doc := range vmessAgreementCorpus {
		payload := base64.StdEncoding.EncodeToString([]byte(doc))

		wantMap, wantOK := decodeVmessJSON(payload)
		decoded, decodeOK := decodeVmessPayload(payload)
		if !decodeOK {
			t.Fatalf("%q: payload did not base64-decode", doc)
		}
		got, gotOK := vmessFields(decoded)

		if gotOK != wantOK {
			t.Errorf("%q: vmessFields ok = %v, map decode ok = %v", doc, gotOK, wantOK)
			continue
		}
		if !wantOK {
			continue
		}
		for _, f := range []struct {
			key string
			got []byte
		}{{"add", got.add}, {"port", got.port}, {"ps", got.ps}} {
			if have, want := jsonValueString(f.got), jsonValueString(wantMap[f.key]); have != want {
				t.Errorf("%q: %s = %q, map decode = %q", doc, f.key, have, want)
			}
		}
	}
}

// TestVmessFieldsAllocatesNothing pins the reason the walker exists. The map
// decode it replaced cost a measured 55 allocations per node — one per field of
// a document three fields are read from.
func TestVmessFieldsAllocatesNothing(t *testing.T) {
	doc := []byte(vmessAgreementCorpus[len(vmessAgreementCorpus)-1])
	if allocs := testing.AllocsPerRun(100, func() {
		fields, ok := vmessFields(doc)
		if !ok || len(fields.add) == 0 {
			t.Fatal("fixture must decode")
		}
	}); allocs != 0 {
		t.Fatalf("vmessFields allocated %.0f times per call, want 0", allocs)
	}
}

// TestRewriteVmessNameMatchesMapRoundTrip is the equivalence proof for the
// splice: over every corpus document it must accept exactly what the
// map-and-remarshal form accepted and emit a payload that decodes to the same
// document with "ps" set. Byte equality is deliberately NOT the bar — the splice
// keeps the producer's field order where the map form sorted it — but nothing a
// JSON decoder can see may differ, since mihomo reads the payload as a map.
func TestRewriteVmessNameMatchesMapRoundTrip(t *testing.T) {
	t.Parallel()

	const newName = "[GEO:FI][IP:192.0.2.1] spliced 001"
	for _, doc := range vmessAgreementCorpus {
		line := "vmess://" + base64.StdEncoding.EncodeToString([]byte(doc))

		got, gotOK := RewriteVmessName(line, newName)
		want, wantOK := mapRoundTripVmess(t, doc, newName)
		if gotOK != wantOK {
			t.Errorf("%q: RewriteVmessName ok = %v, map round trip ok = %v", doc, gotOK, wantOK)
			continue
		}
		if !gotOK {
			continue
		}
		if have := decodeVmessDocument(t, got); !reflect.DeepEqual(have, want) {
			t.Errorf("%q: spliced payload decodes to %v, map round trip to %v", doc, have, want)
		}
	}
}

// TestRewriteVmessNameInsertsMissingPs covers the shapes the agreement corpus
// leaves out of the splice's insert branch: an object with no "ps" that is
// empty, whitespace-padded, or has members the inserted one needs a comma from.
func TestRewriteVmessNameInsertsMissingPs(t *testing.T) {
	t.Parallel()

	const newName = "inserted 001"
	for _, doc := range []string{
		`{}`, `{ }`, "\n\t{ \"add\" : \"x\" }\n", `{"add":"x","port":443}`,
		`{"deep":{"a":[1,2]},"add":"x"}`,
	} {
		line := "vmess://" + base64.StdEncoding.EncodeToString([]byte(doc))
		got, ok := RewriteVmessName(line, newName)
		if !ok {
			t.Errorf("%q: rewrite refused a document the map form accepts", doc)
			continue
		}
		want, _ := mapRoundTripVmess(t, doc, newName)
		if have := decodeVmessDocument(t, got); !reflect.DeepEqual(have, want) {
			t.Errorf("%q: spliced payload decodes to %v, map round trip to %v", doc, have, want)
		}
	}
}

// mapRoundTripVmess is the map-and-remarshal rewrite the splice replaced, kept
// here as the oracle it is pinned against.
func mapRoundTripVmess(t *testing.T, doc, newName string) (map[string]any, bool) {
	t.Helper()

	m, ok := decodeVmessJSON(base64.StdEncoding.EncodeToString([]byte(doc)))
	if !ok {
		return nil, false
	}
	nameJSON, err := json.Marshal(newName)
	if err != nil {
		t.Fatalf("marshal %q: %v", newName, err)
	}
	m["ps"] = nameJSON
	out, err := json.Marshal(m)
	if err != nil {
		return nil, false
	}

	var want map[string]any
	if err = json.Unmarshal(out, &want); err != nil {
		t.Fatalf("unmarshal round trip of %q: %v", doc, err)
	}

	return want, true
}

func decodeVmessDocument(t *testing.T, line string) map[string]any {
	t.Helper()

	plain, err := base64.StdEncoding.DecodeString(line[len("vmess://"):])
	if err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	var m map[string]any
	if err = json.Unmarshal(plain, &m); err != nil {
		t.Fatalf("unmarshal %q: %v", plain, err)
	}

	return m
}
