// Package statusproto defines the aggregate counts shared with the public status reader.
package statusproto

import (
	"encoding/json"
	"fmt"
	"time"
)

type Counts struct {
	Packages     int64         `json:"packages"`
	Ahead        int64         `json:"ahead"`
	LocalBuilds  *int64        `json:"local_builds,omitempty"`
	Compilations *Compilations `json:"compilations,omitempty"`
}

type Compilations struct {
	UTCDate   int   `json:"utc_date"`
	Today     int64 `json:"today"`
	Yesterday int64 `json:"yesterday"`
	Last7Days int64 `json:"last_7_days"`
}

func UTCDate(t time.Time) int {
	year, month, day := t.UTC().Date()
	return year*10000 + int(month)*100 + day
}

// RecordSize leaves room for every counter at its maximum width.
const RecordSize = 256

// Keep one fixed-width record so a single pwrite replaces it in place,
// including when optional counts disappear or the numbers shrink.
func (c Counts) Record() []byte {
	data, _ := json.Marshal(c)
	return fmt.Appendf(nil, "%-*s\n", RecordSize-1, data)
}

func Parse(record []byte) (Counts, bool) {
	// Use sentinel values to distinguish absent counters from real zeros.
	wire := struct {
		Counts
		Compilations json.RawMessage `json:"compilations"`
	}{Counts: Counts{Packages: -1, Ahead: -1}}
	if err := json.Unmarshal(record, &wire); err != nil {
		return Counts{}, false
	}
	counts := wire.Counts
	if counts.Packages < 0 || counts.Ahead < 0 || counts.Ahead > counts.Packages ||
		counts.LocalBuilds != nil && (*counts.LocalBuilds < 0 || *counts.LocalBuilds > counts.Packages) {
		return Counts{}, false
	}
	if len(wire.Compilations) != 0 {
		c := Compilations{Today: -1, Yesterday: -1, Last7Days: -1}
		if err := json.Unmarshal(wire.Compilations, &c); err != nil || c.UTCDate <= 0 ||
			c.Today < 0 || c.Yesterday < 0 || c.Last7Days < c.Today || c.Last7Days-c.Today < c.Yesterday {
			return Counts{}, false
		}
		counts.Compilations = &c
	}
	return counts, true
}
