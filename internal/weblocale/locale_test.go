package weblocale

import (
	"net/http/httptest"
	"testing"
)

func TestRequestLocale(t *testing.T) {
	for _, tt := range []struct {
		headers []string
		want    string
	}{
		{nil, "en"},
		{[]string{"fr;q=0.3", "hi-IN;q=0.9, de;q=0"}, "hi-IN"},
		{[]string{"de-DE;q=0.8, fr;q=0.8"}, "de-DE"},
		{[]string{"*, x-private, en;q=0, de;q=0.4"}, "de"},
		{[]string{"en-u-nu-arab"}, "en-u-nu-arab"},
		{[]string{"invalid tag, pt-br;q=0.5"}, "pt-BR"},
		{[]string{"invalid tag, fr;q=NaN, de;q=2, ta;q=0"}, "en"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		for _, header := range tt.headers {
			r.Header.Add("Accept-Language", header)
		}
		if got := ForRequest(r).String(); got != tt.want {
			t.Errorf("Accept-Language %q: got %q, want %q", tt.headers, got, tt.want)
		}
	}
}
