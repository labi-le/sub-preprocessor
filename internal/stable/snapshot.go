package stable

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/rs/zerolog"
)

// snapshotFileMode keeps the file private to the service uid: /stable.txt is
// the supported way to read the list, and the file is only a restart cache.
const snapshotFileMode os.FileMode = 0o600

// snapshotFile is the on-disk form of a Snapshot. Payload is a string rather
// than []byte because the list is UTF-8 text and encoding/json would base64 a
// byte slice, turning a document an operator can read into one they cannot.
type snapshotFile struct {
	Payload   string    `json:"payload"`
	UpdatedAt time.Time `json:"updated_at"`
	Stats     Stats     `json:"stats"`
}

// snapshotTail is snapshotFile without the payload field, marshalled for the
// streamed document (see SaveSnapshot): marshalling snapshotFile with an empty
// Payload would splice a second, empty "payload" key into the output.
type snapshotTail struct {
	UpdatedAt time.Time `json:"updated_at"`
	Stats     Stats     `json:"stats"`
}

// LoadSnapshot reads the list persisted by the last publish so /stable.txt can
// serve it from the first request. It never fails a startup: an empty path
// (persistence off), a missing file, an unreadable one and a malformed one all
// return nil, which leaves the holder empty and /stable.txt answering 503
// exactly as it did before this file existed.
//
// There is no TTL. That mirrors the in-memory rule -- the last good list is
// kept when a cycle fails -- so a restored list is never staler than what a run
// of failing cycles already serves, and its age stays visible in the
// X-Stable-Stats updated= field.
func LoadSnapshot(path string, logger zerolog.Logger) *Snapshot {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		logger.Warn().Err(err).Str("path", path).
			Msg("no stable snapshot restored; /stable.txt answers 503 until the first cycle completes")

		return nil
	}
	var f snapshotFile
	if err = json.Unmarshal(b, &f); err != nil {
		logger.Warn().Err(err).Str("path", path).
			Msg("stable snapshot is malformed; /stable.txt answers 503 until the first cycle completes")

		return nil
	}
	// A cycle never publishes an empty list (RunOnce skips the Store when no
	// survivor is left), so a file carrying one was not written by us, and
	// serving it would answer 200 with nothing in the body.
	if f.Payload == "" {
		logger.Warn().Str("path", path).
			Msg("stable snapshot carries an empty list; /stable.txt answers 503 until the first cycle completes")

		return nil
	}
	logger.Info().Str("path", path).Time("updated_at", f.UpdatedAt).Int("kept", f.Stats.Kept).
		Msg("restored stable list from snapshot")

	return &Snapshot{Payload: []byte(f.Payload), UpdatedAt: f.UpdatedAt, Stats: f.Stats}
}

// SaveSnapshot persists s, or does nothing when path is empty. The document is
// written to a temp file in the SAME directory and renamed over path, so a
// concurrent reader observes either the previous snapshot or this one, never a
// partial document, and a failed write leaves the previous one in place.
//
// The payload field is streamed into the temp file rather than marshalled into
// a whole-document buffer: holding the payload as a string copy plus the
// escaped document (which doubling-ladder growth reallocates several times)
// transiently quadrupled the payload once per published cycle for no benefit.
// The small tail (updated_at, stats) is marshalled alone; the payload passes
// through only writeSnapshotPayload's bounded scratch buffer.
func SaveSnapshot(path string, s *Snapshot) error {
	if path == "" {
		return nil
	}
	// {"updated_at":...,"stats":{...}} once, not per payload byte; the leading
	// '{' is dropped when the head+payload+tail document is spliced.
	tail, err := json.Marshal(snapshotTail{UpdatedAt: s.UpdatedAt, Stats: s.Stats})
	if err != nil {
		return fmt.Errorf("marshal snapshot tail: %w", err)
	}

	return writeSnapshotAtomic(path, func(f *os.File) error {
		return writeSnapshotDoc(f, s, tail)
	})
}

// writeSnapshotDoc splices the document around the streamed payload: head,
// escaped payload, then the marshalled tail minus its leading '{'.
func writeSnapshotDoc(f *os.File, s *Snapshot, tail []byte) error {
	if _, err := f.WriteString(`{"payload":"`); err != nil {
		return fmt.Errorf("write document head: %w", err)
	}
	if err := writeSnapshotPayload(f, s.Payload); err != nil {
		return err
	}
	if _, err := f.WriteString(`",`); err != nil {
		return fmt.Errorf("write document splice: %w", err)
	}
	if _, err := f.Write(tail[1:]); err != nil {
		return fmt.Errorf("write document tail: %w", err)
	}

	return nil
}

// writeSnapshotPayload writes payload as a JSON string body, escaping only
// what the JSON grammar requires (", \ and bytes below 0x20). encoding/json
// additionally escapes <, >, & and U+2028/U+2029, which the file's only
// reader, LoadSnapshot, decodes identically, so the smaller document is
// chosen. The scratch buffer bounds the marshal-side allocation to ~64 KiB
// regardless of payload size; runs between escaped bytes are written whole.
func writeSnapshotPayload(f *os.File, payload []byte) error {
	const maxScratch = 64 << 10
	scratch := make([]byte, 0, maxScratch)
	run := 0
	for i := range payload {
		esc, n, needs := jsonEscape(payload[i])
		if !needs {
			continue
		}
		var err error
		if scratch, err = writePayloadChunk(f, scratch, maxScratch, payload[run:i]); err != nil {
			return err
		}
		if scratch, err = writePayloadChunk(f, scratch, maxScratch, esc[:n]); err != nil {
			return err
		}
		run = i + 1
	}
	scratch, err := writePayloadChunk(f, scratch, maxScratch, payload[run:])
	if err != nil {
		return err
	}

	return flushPayload(f, &scratch)
}

// flushPayload writes the pending buffer to f and clears it.
func flushPayload(f *os.File, scratch *[]byte) error {
	if len(*scratch) == 0 {
		return nil
	}
	if _, err := f.Write(*scratch); err != nil {
		return fmt.Errorf("flush payload buffer: %w", err)
	}
	*scratch = (*scratch)[:0]
	return nil
}

// writePayloadChunk appends b to the scratch buffer, flushing to f when the
// buffer would overflow; a run bigger than the whole buffer is written
// directly. It returns the (possibly reallocated) buffer.
func writePayloadChunk(f *os.File, scratch []byte, maxScratch int, b []byte) ([]byte, error) {
	if len(b) >= maxScratch {
		if err := flushPayload(f, &scratch); err != nil {
			return scratch, err
		}
		if _, err := f.Write(b); err != nil {
			return scratch, fmt.Errorf("write payload run: %w", err)
		}
		return scratch, nil
	}
	if len(scratch)+len(b) > maxScratch {
		if err := flushPayload(f, &scratch); err != nil {
			return scratch, err
		}
	}

	return append(scratch, b...), nil
}

// jsonControlMax is the highest byte the JSON grammar forces an escape for
// (the short forms above cover the common ones).
const jsonControlMax = 0x1F

// jsonEscape returns the JSON string escape for b, or needs == false when b
// may be written raw.
func jsonEscape(b byte) (esc [6]byte, n int, needs bool) {
	switch b {
	case '"', '\\':
		esc[0], esc[1], n = '\\', b, 2
	case '\b':
		esc[0], esc[1], n = '\\', 'b', 2
	case '\f':
		esc[0], esc[1], n = '\\', 'f', 2
	case '\n':
		esc[0], esc[1], n = '\\', 'n', 2
	case '\r':
		esc[0], esc[1], n = '\\', 'r', 2
	case '\t':
		esc[0], esc[1], n = '\\', 't', 2
	default:
		if b > jsonControlMax {
			return esc, 0, false
		}
		const hex = "0123456789abcdef"
		esc = [6]byte{'\\', 'u', '0', '0', hex[b>>4], hex[b&0xf]}
		n = 6
	}
	return esc, n, true
}

// writeSnapshotAtomic performs the temp-write/fsync/rename dance. The temp file
// is a sibling so the rename stays within one filesystem, and it is removed on
// every failure path -- after a successful rename that name is gone and the
// removal is a no-op.
func writeSnapshotAtomic(path string, write func(*os.File) error) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, snapshotFileMode)
	if err != nil {
		return fmt.Errorf("open temp: %w", err)
	}
	defer func() { _ = os.Remove(tmp) }()

	if err = write(f); err != nil {
		_ = f.Close()

		return fmt.Errorf("write temp: %w", err)
	}
	// The rename is what publishes the file; without the sync a host crash can
	// publish a name whose content never reached the device.
	if err = f.Sync(); err != nil {
		_ = f.Close()

		return fmt.Errorf("sync temp: %w", err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}

	return nil
}
