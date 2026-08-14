package subscription

import (
	"encoding/base64"
	"testing"
)

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
// replaced the map decode on the parse path. The map form is still live under
// RewriteVmessName, so the two must keep answering identically for every
// document: a walker that accepted one more shape would keep nodes mihomo drops,
// and one that accepted fewer would drop nodes it converts.
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
