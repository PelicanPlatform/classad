package changefeed

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db/replicate"
)

// Event is the NDJSON wire form of a replicate.Change (one JSON object per line). It is what an
// external, possibly non-Go, sink parses. Ad is a JSON object present only on an upsert; Cursor
// marshals as base64 (encoding/json's default for []byte).
type Event struct {
	Kind   replicate.Kind  `json:"kind"`
	Src    string          `json:"src,omitempty"`
	Ver    uint64          `json:"ver,omitempty"`
	Key    string          `json:"key,omitempty"`
	Ad     json.RawMessage `json:"ad,omitempty"`
	Cursor []byte          `json:"cursor,omitempty"`
	TS     int64           `json:"ts,omitempty"`

	FromMillis int64 `json:"fromMillis,omitempty"`
	ToMillis   int64 `json:"toMillis,omitempty"`
}

// ToEvent renders a Change as its wire Event (marshaling the ad to JSON).
func ToEvent(c replicate.Change) (Event, error) {
	e := Event{Kind: c.Kind, Src: c.Src, Ver: c.Ver, Key: c.Key, Cursor: c.Cursor, TS: c.TS, FromMillis: c.FromMillis, ToMillis: c.ToMillis}
	if c.Kind == replicate.KindUpsert && c.Ad != nil {
		b, err := c.Ad.MarshalJSON()
		if err != nil {
			return Event{}, fmt.Errorf("changefeed: marshaling ad for %s: %w", c.Key, err)
		}
		e.Ad = b
	}
	return e, nil
}

// ToChange parses a wire Event back into an in-memory Change (unmarshaling the ad JSON).
func ToChange(e Event) (replicate.Change, error) {
	c := replicate.Change{Kind: e.Kind, Src: e.Src, Ver: e.Ver, Key: e.Key, Cursor: e.Cursor, TS: e.TS, FromMillis: e.FromMillis, ToMillis: e.ToMillis}
	if e.Kind == replicate.KindUpsert && len(e.Ad) > 0 {
		ad := classad.New()
		if err := ad.UnmarshalJSON(e.Ad); err != nil {
			return replicate.Change{}, fmt.Errorf("changefeed: parsing ad for %s: %w", e.Key, err)
		}
		c.Ad = ad
	}
	return c, nil
}

// WriteEvent encodes one Event as an NDJSON line (a trailing newline, no embedded newlines since
// json.Marshal escapes them). Suitable as one SSE data payload or one line of a chunked stream.
func WriteEvent(w io.Writer, e Event) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// DecodeEvents streams Events from NDJSON r, calling yield for each. It stops on the first yield
// returning false, on EOF (nil), or on a read/parse error. Blank lines are skipped.
func DecodeEvents(r io.Reader, yield func(Event) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // large ads
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			return fmt.Errorf("changefeed: decoding event: %w", err)
		}
		if !yield(e) {
			return nil
		}
	}
	return sc.Err()
}
