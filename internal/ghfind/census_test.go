package ghfind_test

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash/fnv"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/ghfind"
)

// TestEndpointHash pins host case-insensitivity, verbatim port digits and the
// FNV-1a construction: the hash must equal the stdlib FNV-1a over the
// canonical lowercase "server:port" form.
func TestEndpointHash(t *testing.T) {
	t.Parallel()

	lower := ghfind.EndpointHash("example.com", "443")
	if got := ghfind.EndpointHash("EXAMPLE.COM", "443"); got != lower {
		t.Fatalf("host case must not change the hash: got %d, want %d", got, lower)
	}
	if got := ghfind.EndpointHash("eXaMpLe.CoM", "443"); got != lower {
		t.Fatalf("mixed-case host = %d, want %d", got, lower)
	}
	if got := ghfind.EndpointHash("example.com", "8443"); got == lower {
		t.Fatal("a different port must hash differently")
	}
	if got := ghfind.EndpointHash("example.com", "0443"); got == lower {
		t.Fatal("port digits must hash verbatim: 0443 is not 443")
	}

	f := fnv.New64a()
	_, _ = f.Write([]byte("example.com:443"))
	if got := ghfind.EndpointHash("eXaMpLe.CoM", "443"); got != f.Sum64() {
		t.Fatalf("EndpointHash = %d, stdlib FNV-1a over %q = %d", got, "example.com:443", f.Sum64())
	}
}

// TestEndpointsSortedDeduped feeds one body mixing a base64 vmess node, two
// spellings of the same vless host (one uppercase, proving the fold) and a
// loopback placeholder that must be skipped; the result must be the sorted,
// deduplicated hash pair.
func TestEndpointsSortedDeduped(t *testing.T) {
	t.Parallel()

	payload := `{"v":"2","ps":"eu-01","add":"example.org","port":"8443","id":"11111111-1111-1111-1111-111111111111"}`
	vmess := "vmess://" + base64.StdEncoding.EncodeToString([]byte(payload))
	body := []byte("vless://11111111-1111-1111-1111-111111111111@example.com:443#one\n" +
		"vless://11111111-1111-1111-1111-111111111111@EXAMPLE.com:443#two\n" +
		"vless://11111111-1111-1111-1111-111111111111@localhost:443#loopback\n" +
		vmess + "\n")

	got := ghfind.Endpoints(body, nil)
	want := []uint64{
		ghfind.EndpointHash("example.com", "443"),
		ghfind.EndpointHash("example.org", "8443"),
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("Endpoints = %v, want %v (sorted, deduplicated, placeholders skipped)", got, want)
	}

	// The scratch form must reuse the caller's capacity, not grow past need.
	scratch := make([]uint64, 10)
	again := ghfind.Endpoints(body, scratch)
	if !slices.Equal(again, want) {
		t.Fatalf("Endpoints(scratch) = %v, want %v", again, want)
	}
}

// TestAddBodyNewCount pins the new-only accounting: the first fold reports
// every endpoint, the second reports none, and the census holds the union.
func TestAddBodyNewCount(t *testing.T) {
	t.Parallel()

	body := []byte("vless://11111111-1111-1111-1111-111111111111@example.com:443#a\n" +
		"vless://11111111-1111-1111-1111-111111111111@example.org:8443#b\n")
	c := ghfind.NewCensus(2)
	if got := c.AddBody(body); got != 2 {
		t.Fatalf("first AddBody = %d, want 2", got)
	}
	if got := c.AddBody(body); got != 0 {
		t.Fatalf("second AddBody = %d, want 0", got)
	}
	if c.Len() != 2 {
		t.Fatalf("Len = %d, want 2", c.Len())
	}
	for _, h := range []uint64{
		ghfind.EndpointHash("example.com", "443"),
		ghfind.EndpointHash("example.org", "8443"),
	} {
		if !c.Has(h) {
			t.Fatalf("census lost endpoint %d", h)
		}
	}
}

// TestBuildCensusFolds2xxSkipsFailures runs the build against a local server
// serving two distinct bodies (one duplicating an endpoint under an
// uppercase host) and one 500: the census must hold the two distinct
// endpoints and the build must not fail on the downed URL.
func TestBuildCensusFolds2xxSkipsFailures(t *testing.T) {
	t.Parallel()

	bodyA := []byte("vless://11111111-1111-1111-1111-111111111111@example.com:443#a\n")
	bodyB := []byte("vless://11111111-1111-1111-1111-111111111111@EXAMPLE.com:443#dup\n" +
		"vless://11111111-1111-1111-1111-111111111111@example.org:8443#b\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a":
			_, _ = w.Write(bodyA)
		case "/b":
			_, _ = w.Write(bodyB)
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c, err := ghfind.BuildCensus(context.Background(), srv.Client(),
		[]string{srv.URL + "/a", srv.URL + "/b", srv.URL + "/down"}, 2, zerolog.Nop())
	if err != nil {
		t.Fatalf("BuildCensus: %v", err)
	}
	if c.Len() != 2 {
		t.Fatalf("census Len = %d, want 2", c.Len())
	}
	for _, h := range []uint64{
		ghfind.EndpointHash("example.com", "443"),
		ghfind.EndpointHash("example.org", "8443"),
	} {
		if !c.Has(h) {
			t.Fatalf("census lost endpoint %d", h)
		}
	}
}

// TestBuildCensusAllFailed pins the sentinel: when every URL fails — or none
// is given — the build reports ErrCensusBuild instead of returning a census
// that would make every endpoint look novel.
func TestBuildCensusAllFailed(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := srv.Client()
	if _, err := ghfind.BuildCensus(context.Background(), client,
		[]string{srv.URL + "/one", srv.URL + "/two"}, 2, zerolog.Nop()); !errors.Is(err, ghfind.ErrCensusBuild) {
		t.Fatalf("all-URLs-failed error = %v, want ErrCensusBuild", err)
	}
	if _, err := ghfind.BuildCensus(context.Background(), client, nil, 2, zerolog.Nop()); !errors.Is(err, ghfind.ErrCensusBuild) {
		t.Fatalf("no-URLs error = %v, want ErrCensusBuild", err)
	}
}

// censusBody returns a body whose endpoints are the given server:port pairs.
func censusBody(pairs ...string) []byte {
	body := []byte("vless://11111111-1111-1111-1111-111111111111@" + pairs[0] + "#n\n")
	for _, p := range pairs[1:] {
		body = append(body, []byte("vless://11111111-1111-1111-1111-111111111111@"+p+"#n\n")...)
	}
	return body
}

// TestCensusSaveLoadRoundTrip saves a census, loads it back and checks every
// endpoint survived and the file's mtime came back as the census age.
func TestCensusSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()

	c := ghfind.NewCensus(4)
	c.AddBody(censusBody("example.com:443", "example.org:8443", "example.net:2082"))

	path := filepath.Join(t.TempDir(), "census.bin")
	if err := ghfind.SaveCensus(path, c); err != nil {
		t.Fatalf("SaveCensus: %v", err)
	}
	got, at, err := ghfind.LoadCensus(path)
	if err != nil {
		t.Fatalf("LoadCensus: %v", err)
	}
	if at.IsZero() {
		t.Fatal("LoadCensus must return the file's mtime as the census age")
	}
	if got.Len() != 3 {
		t.Fatalf("loaded Len = %d, want 3", got.Len())
	}
	for _, h := range []uint64{
		ghfind.EndpointHash("example.com", "443"),
		ghfind.EndpointHash("example.org", "8443"),
		ghfind.EndpointHash("example.net", "2082"),
	} {
		if !got.Has(h) {
			t.Fatalf("round trip lost endpoint %d", h)
		}
	}

	gone, builtAt, goneErr := ghfind.LoadCensus(filepath.Join(t.TempDir(), "absent.bin"))
	if goneErr != nil || gone != nil || !builtAt.IsZero() {
		t.Fatalf("LoadCensus(missing) = %v, %v, %v; want nil, zero time, nil", gone, builtAt, goneErr)
	}
}

// TestLoadCensusRefusesForeignFiles pins the refusal sentinel for a file that
// is not a version-1 census: wrong magic, an unknown format version, and a
// valid file truncated mid-record.
func TestLoadCensusRefusesForeignFiles(t *testing.T) {
	t.Parallel()

	const (
		recordSize = 8
		headerSize = 2 * recordSize
	)
	path := filepath.Join(t.TempDir(), "census.bin")
	write := func(b []byte) {
		t.Helper()
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	load := func() error {
		t.Helper()
		_, _, err := ghfind.LoadCensus(path)
		return err
	}

	write([]byte("this is not a census file, not even close"))
	if err := load(); !errors.Is(err, ghfind.ErrCensusCorrupt) {
		t.Fatalf("bad magic: error = %v, want ErrCensusCorrupt", err)
	}

	// The header layout is pinned here so a format change fails loudly:
	// magic "ECNS" and a version this reader does not know.
	foreign := make([]byte, headerSize)
	binary.LittleEndian.PutUint64(foreign[0:recordSize], uint64(0x534E4345)|uint64(99)<<32)
	binary.LittleEndian.PutUint64(foreign[recordSize:headerSize], 0)
	write(foreign)
	if err := load(); !errors.Is(err, ghfind.ErrCensusCorrupt) {
		t.Fatalf("unknown version: error = %v, want ErrCensusCorrupt", err)
	}

	good := filepath.Join(t.TempDir(), "good.bin")
	c := ghfind.NewCensus(1)
	c.AddBody(censusBody("example.com:443"))
	if err := ghfind.SaveCensus(good, c); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	write(raw[:len(raw)-recordSize/2]) // chop the last record mid-way
	if err = load(); !errors.Is(err, ghfind.ErrCensusCorrupt) {
		t.Fatalf("truncated records: error = %v, want ErrCensusCorrupt", err)
	}
}
