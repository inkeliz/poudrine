package broker

import (
	"crypto/ed25519"
	"net/http"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/testutil"
)

func brokerRequest(t *testing.T, k ed25519.PrivateKey, method, path string, body []byte) *http.Request {
	t.Helper()
	return testutil.SignedRequest(t, k, method, path, body, time.Now())
}
