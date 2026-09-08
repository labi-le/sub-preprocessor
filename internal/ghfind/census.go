package ghfind

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/fetch"
	"domains.lst/sub-preprocessor/internal/subscription"
)

// Census is the novelty baseline of the discovery phase: a set of endpoint
// hashes (EndpointHash over server:port) covering the whole shipped corpus.
// Only 64-bit hashes are kept, never endpoint strings. Use NewCensus; the
// zero value is not ready. The methods are not synchronized; BuildCensus
// serialises its folds with one mutex.
type Census struct {
	m map[uint64]struct{}
}

func NewCensus(hint int) *Census {
	return &Census{m: make(map[uint64]struct{}, hint)}
}

func (c *Census) Add(h uint64) {
	c.m[h] = struct{}{}
}

func (c *Census) Has(h uint64) bool {
	_, ok := c.m[h]
	return ok
}

func (c *Census) Len() int {
	return len(c.m)
}

// AddBody folds every endpoint of one body into the census and reports how
// many were new. Nodes come from the same subscription.Normalize and Parse
// the verdict path reaches, so the baseline and a candidate's novelty count
// can never disagree about a node; placeholders and empty servers are
// skipped exactly as Endpoints skips them.
func (c *Census) AddBody(body []byte) int {
	added := 0
	parseEndpoints(body, func(h uint64) {
		if _, ok := c.m[h]; !ok {
			c.m[h] = struct{}{}
			added++
		}
	})
	return added
}

// Endpoints returns the sorted, deduplicated endpoint hashes of one body,
// appended into the caller's scratch dst (pass nil for a fresh slice). It is
// the hot per-candidate path: one parse pass, no per-node allocation, no
// map. The returned slice shares backing with dst.
func Endpoints(body []byte, dst []uint64) []uint64 {
	dst = dst[:0]
	parseEndpoints(body, func(h uint64) {
		dst = append(dst, h)
	})
	slices.Sort(dst)
	return slices.Compact(dst)
}

// EndpointHash is an endpoint's identity: FNV-1a 64 over the lowercased
// server, a colon and the port, folded byte by byte so nothing is allocated
// and no string is built. Host case must not split one endpoint in two;
// ports are digits and pass through verbatim, so "443" and "0443" are
// distinct hashes (Parse refuses the latter anyway).
func EndpointHash(server, port string) uint64 {
	const (
		fnvOffset = 14695981039346656037
		fnvPrime  = 1099511628211
		colon     = ':'
	)
	h := uint64(fnvOffset)
	for i := range len(server) {
		c := server[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		h ^= uint64(c)
		h *= fnvPrime
	}
	h ^= colon
	h *= fnvPrime
	for i := range len(port) {
		h ^= uint64(port[i])
		h *= fnvPrime
	}
	return h
}

// parseEndpoints walks one body's endpoints in parse order, calling yield
// once per hash. Placeholder nodes and empty servers are skipped: a
// placeholder is a panel's stand-in for a subscription it no longer serves,
// so it must not pollute the baseline or count as novel.
func parseEndpoints(body []byte, yield func(uint64)) {
	norm := subscription.Normalize(body)
	subscription.Parse(norm, func(n subscription.Node) bool {
		if n.Server == "" || subscription.PlaceholderNode(n.Raw, n.Server) {
			return true
		}
		yield(EndpointHash(n.Server, n.Port))
		return true
	})
}

// censusPerSourceHint presizes the census map. Two measurements bracket the
// density: 73 774 endpoints over 1058 sources on 2026-09-07 (69.7) and 74 344
// over 1096 in the first production run (67.8). The hint sits above both
// because Go rounds it up through the load factor to a bucket count: a hint
// under the real total is free until it falls a step short, and then the whole
// map grows mid-build. At today's corpus 64 and 72 pick the same bucket count;
// 72 is what keeps that true if the corpus halves.
const censusPerSourceHint = 72

// censusFetchTimeout caps one source fetch so a hung origin cannot pin a
// worker for the whole build; it matches fetch's own client timeout.
const censusFetchTimeout = 30 * time.Second

// ErrCensusBuild reports that no census baseline could be built: no source
// URL was given, or every one of them failed. A caller must treat it like a
// nil census — never accept anything against a baseline that does not exist.
var ErrCensusBuild = errors.New("endpoint census: no source succeeded")

// ErrCensusCorrupt reports a census file that is not a valid census: a
// foreign header (bad magic or format version) or a truncated record stream.
// The file is refused whole, because a half-read baseline would silently
// mark the endpoints that were cut off as novel.
var ErrCensusCorrupt = errors.New("endpoint census: corrupt or foreign file")

// Census file layout, little-endian:
//
//	[0:8]  magic "ECNS" and format version 1, in one uint64
//	[8:16] record count (uint64)
//	[16:]  count endpoint hashes (uint64 each), ascending
//
// Magic and version share one word, so a foreign header and a version this
// reader does not know are refused by one compare. SaveCensus writes
// ascending; the count must equal the bytes after the header exactly, which
// refuses a truncated record stream and trailing garbage alike.
const (
	censusRecordSize   = 8
	censusHeaderSize   = 2 * censusRecordSize
	censusMagicVersion = uint64(0x534E4345) | uint64(1)<<32 // "ECNS", version 1
)

// SaveCensus writes the census to path atomically: a 0600 temp file in the
// same directory (os.CreateTemp's mode), synced, then renamed over the
// target, so a crash can never leave a half-written census where LoadCensus
// will find it. Hashes are stored ascending, so equal censuses write equal
// files.
func SaveCensus(path string, c *Census) error {
	keys := make([]uint64, 0, len(c.m))
	for h := range c.m {
		keys = append(keys, h)
	}
	slices.Sort(keys)

	buf := make([]byte, censusHeaderSize+censusRecordSize*len(keys))
	binary.LittleEndian.PutUint64(buf[0:censusRecordSize], censusMagicVersion)
	binary.LittleEndian.PutUint64(buf[censusRecordSize:censusHeaderSize], uint64(len(keys)))
	for i, h := range keys {
		binary.LittleEndian.PutUint64(buf[censusHeaderSize+censusRecordSize*i:], h)
	}

	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("create census temp file: %w", err)
	}
	name := f.Name()
	discard := func() {
		_ = f.Close()
		_ = os.Remove(name)
	}
	if _, err = f.Write(buf); err != nil {
		discard()
		return fmt.Errorf("write census file: %w", err)
	}
	if err = f.Sync(); err != nil {
		discard()
		return fmt.Errorf("sync census file: %w", err)
	}
	if err = f.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("close census file: %w", err)
	}
	if err = os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("rename census file: %w", err)
	}
	return nil
}

// LoadCensus reads a census written by SaveCensus and reports its age as the
// file's modification time. A missing file is (nil, zero time, nil) — the
// caller builds a fresh census. Any file that is not a valid census — a
// foreign header, a lying record count, or records truncated at any byte —
// is refused with ErrCensusCorrupt rather than half-read.
func LoadCensus(path string) (*Census, time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, time.Time{}, nil
		}
		return nil, time.Time{}, fmt.Errorf("open census file: %w", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("stat census file: %w", err)
	}
	if fi.Size() < censusHeaderSize {
		return nil, time.Time{}, ErrCensusCorrupt
	}
	var header [censusHeaderSize]byte
	if _, err = io.ReadFull(f, header[:]); err != nil {
		return nil, time.Time{}, fmt.Errorf("read census header: %w", err)
	}
	if binary.LittleEndian.Uint64(header[0:censusRecordSize]) != censusMagicVersion {
		return nil, time.Time{}, ErrCensusCorrupt
	}
	count := binary.LittleEndian.Uint64(header[censusRecordSize:censusHeaderSize])
	payload := fi.Size() - censusHeaderSize
	if payload%censusRecordSize != 0 || uint64(payload/censusRecordSize) != count {
		return nil, time.Time{}, ErrCensusCorrupt
	}
	data := make([]byte, payload)
	if _, err = io.ReadFull(f, data); err != nil {
		return nil, time.Time{}, fmt.Errorf("read census records: %w", err)
	}
	c := NewCensus(int(count))
	for i := 0; i < len(data); i += censusRecordSize {
		c.m[binary.LittleEndian.Uint64(data[i:])] = struct{}{}
	}
	return c, fi.ModTime(), nil
}

// BuildCensus fetches every source URL with a bounded worker pool and folds
// the endpoints of each 2xx body into one census. Individual failures are
// logged at debug and skipped — one downed source must not sink the
// baseline — so an error is returned only when the context is done (wrapped)
// or when no URL succeeded (ErrCensusBuild). The client is the caller's, so
// the dial and IP policy is the caller's; a non-2xx response is skipped like
// any other failure.
func BuildCensus(ctx context.Context, client *http.Client, urls []string, concurrency int, logger zerolog.Logger) (*Census, error) {
	if len(urls) == 0 {
		return nil, ErrCensusBuild
	}
	if concurrency < 1 {
		concurrency = 1
	}
	census := NewCensus(len(urls) * censusPerSourceHint)
	sem := make(chan struct{}, concurrency)
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		failed atomic.Int64
	)
	for _, u := range urls {
		sem <- struct{}{} // bound goroutine creation, not just execution
		wg.Go(func() {
			defer func() { <-sem }()
			if censusURL(ctx, client, u, census, &mu, logger) {
				failed.Add(1)
			}
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("build endpoint census: %w", err)
	}
	if failed.Load() == int64(len(urls)) {
		return nil, ErrCensusBuild
	}
	return census, nil
}

// censusURL fetches one source and folds its body's endpoints into census,
// reporting whether the URL failed. The body is read through a
// subscription.MaxSubscriptionSize+1 cap, exactly as a worker verdict fetch
// reads it, so the folded endpoints are the ones a fetch would have seen.
func censusURL(ctx context.Context, client *http.Client, url string, census *Census, mu *sync.Mutex, logger zerolog.Logger) bool {
	reqCtx, cancel := context.WithTimeout(ctx, censusFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		logger.Debug().Err(err).Str("url", url).Msg("census: source URL refused")
		return true
	}
	req.Header.Set("User-Agent", fetch.UserAgent())
	resp, err := client.Do(req)
	if err != nil {
		logger.Debug().Err(err).Str("url", url).Msg("census: source fetch failed")
		return true
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		logger.Debug().Str("url", url).Int("status", resp.StatusCode).Msg("census: source answered non-2xx; skipped")
		return true
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(subscription.MaxSubscriptionSize)+1))
	if err != nil {
		logger.Debug().Err(err).Str("url", url).Msg("census: source body unreadable")
		return true
	}
	mu.Lock()
	census.AddBody(body)
	mu.Unlock()
	return false
}
