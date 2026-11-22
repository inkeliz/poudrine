package broker

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/inkeliz/poudrine/internal/requestauth"
)

const MethodQuery = requestauth.Query

// Each QUERY has one JSON object. Its exact bytes have already been signed,
// including any whitespace. Refuse unknown fields and trailing values.
func ValidQuery(body []byte, args any) bool {
	body = bytes.TrimLeft(body, " \t\r\n")
	if len(body) == 0 || body[0] != '{' {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	return decoder.Decode(args) == nil && decoder.Decode(&struct{}{}) == io.EOF
}
