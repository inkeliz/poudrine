package poudrine

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAdminAndSupervisorVerifyExactRequestBytes(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const uri = "/api/admin/config"
	for _, tc := range []struct {
		name, method, signed, received string
		valid                          bool
	}{
		{"query body", methodQuery, "{ }", "{ }", true},
		{"literal empty object", "POST", "{}", "{}", true},
		{"UTF-8 and whitespace", "PUT", "{\r\n \"name\": \"café 🛠\" }\n", "{\r\n \"name\": \"café 🛠\" }\n", true},
		{"space removed", "PUT", `{"name": "café"}`, `{"name":"café"}`, false},
		{"Unicode normalization", "PUT", "café", "cafe\u0301", false},
		{"Unicode escape", "PUT", `{"name":"café"}`, `{"name":"caf\u00e9"}`, false},
		{"line ending", "PUT", "{}\r\n", "{}\n", false},
		{"empty versus object", "POST", "{}", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := signedTestRequest(t, priv, tc.method, uri, []byte(tc.signed), time.Now())
			received := []byte(tc.received)
			r.Body = io.NopCloser(bytes.NewReader(received))
			w := httptest.NewRecorder()
			body, _, _, ok := adminProof(w, r, pub)
			if ok != tc.valid || (ok && !bytes.Equal(body, received)) || (!ok && w.Code != http.StatusUnauthorized) {
				t.Fatalf("admin proof returned ok=%t, body=%q, HTTP %d", ok, body, w.Code)
			}
			msg := configMessage{
				Method: r.Method, Path: r.URL.RequestURI(), Body: received,
				Time: r.Header.Get("X-Poudrine-Time"), Nonce: r.Header.Get("X-Poudrine-Nonce"),
				Signature: r.Header.Get("X-Poudrine-Signature"),
			}
			if msg.signed(pub) != tc.valid {
				t.Fatalf("supervisor verification disagreed with expected result %t", tc.valid)
			}
			if !tc.valid {
				return
			}
			msg.Path = "/api/admin/clients"
			if msg.signed(pub) {
				t.Fatal("supervisor accepted a different wire target")
			}
			msg.Path, msg.Method = r.URL.RequestURI(), "DELETE"
			if msg.signed(pub) {
				t.Fatal("supervisor accepted a different HTTP method")
			}
		})
	}
}
