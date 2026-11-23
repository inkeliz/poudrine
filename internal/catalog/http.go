package catalog

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// A configured upstream is trusted, including an explicitly configured LAN
// mirror. Redirects cannot expand that trust to another host or service.
func upstreamClient(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	copy := *client
	prior := client.CheckRedirect
	copy.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) == 0 || len(via) >= 10 {
			return errors.New("too many upstream redirects")
		}
		if prior != nil {
			if err := prior(req, via); err != nil {
				return err
			}
		}
		first, last, next := via[0].URL, via[len(via)-1].URL, req.URL
		if next.User != nil || (next.Scheme != "http" && next.Scheme != "https") ||
			!strings.EqualFold(next.Hostname(), first.Hostname()) || last.Scheme == "https" && next.Scheme != "https" {
			return errors.New("upstream redirect leaves the configured mirror or downgrades HTTPS")
		}
		samePort := urlPort(next) == urlPort(first)
		upgrade := first.Scheme == "http" && urlPort(first) == "80" && next.Scheme == "https" && urlPort(next) == "443"
		if !samePort && !upgrade {
			return errors.New("upstream redirect changes the configured mirror port")
		}
		return nil
	}
	return &copy
}

func urlPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}
