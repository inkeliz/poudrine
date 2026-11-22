// Package statusproto defines the two counts shared with the public status reader.
package statusproto

import (
	"encoding/json"
	"fmt"
)

type Counts struct {
	Packages int64 `json:"packages"`
	Ahead    int64 `json:"ahead"`
}

// Record pads the JSON to the original 63-byte snapshot size so a single
// pwrite replaces both counters, even when the new numbers have fewer digits.
func (c Counts) Record() []byte {
	data, _ := json.Marshal(c) // Counts contains only integers.
	return fmt.Appendf(nil, "%-62s\n", data)
}

func Parse(record []byte) (Counts, bool) {
	counts := Counts{Packages: -1, Ahead: -1}
	if err := json.Unmarshal(record, &counts); err != nil {
		return Counts{}, false
	}
	return counts, counts.Packages >= 0 && counts.Ahead >= 0 && counts.Ahead <= counts.Packages
}
