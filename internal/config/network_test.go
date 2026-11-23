package config

import (
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestNetworkConfigurationRejectsInvalidValues(t *testing.T) {
	for _, tc := range []struct {
		field, value string
	}{
		{"repo_listen", "bad host:8080"},
		{"repo_listen", "[]:8080"},
		{"repo_listen", "[bad.host]:8080"},
		{"repo_listen", "[fe80::1%bad zone]:8080"},
		{"repo_listen", "999.1.1.1:8080"},
		{"web_listen", "https://localhost:8181"},
		{"web_listen", "localhost:+8181"},
		{"web_listen", "[ff02::1]:8181"},
		{"repo_url", "https://packages..example"},
		{"repo_url", "https://-packages.example"},
		{"repo_url", "https://pkg%.example"},
		{"repo_url", "https://*.example"},
		{"repo_url", "https://192.168.1.320"},
		{"repo_url", "https://127.1"},
		{"repo_url", "https://0x7f000001"},
		{"repo_url", "https://[127.0.0.1]"},
		{"repo_url", "https://[::ffff:0.0.0.0]"},
		{"repo_url", "https://[ff02::1]"},
		{"repo_url", "https://[::1]:0"},
		{"web_url", "https://example.org/app/%2e%2e/admin"},
		{"web_url", "https://example.org/app%5Cproxy"},
		{"web_url", "https://example.org/path%0A"},
		{"web_url", "https://[fe80::1%25em0]:8181"},
		{"upstream", "https://user:pw@repo.org/latest"},
		{"upstream", "https://repo.org/latest#"},
		{"upstream", "https://repo.org/latest?"},
		{"trusted_proxy", "0.0.0.0/0"},
		{"trusted_proxy", "::/0"},
		{"trusted_proxy", "::ffff:0.0.0.0/96"},
		{"trusted_proxy", "192.0.2.1/24"},
		{"trusted_proxy", "2001:db8::1/64"},
		{"push_contact", "https:///contact"},
		{"push_contact", "https://admin:pw@example.org/path"},
		{"push_contact", "https://*.example.org/contact"},
		{"push_contact", "http://example.org/contact"},
		{"push_contact", "mailto:admin@example.org?subject=mail"},
		{"push_contact", "Admin <admin@example.org>"},
		{"push_contact", "admin@example.org, other@example.org"},
		{"push_contact", "admin@-bad-domain.example"},
		{"push_contact", "admin\n@example.org"},
		{"push_contact", "missing-domain"},
	} {
		t.Run(tc.field+"/"+tc.value, func(t *testing.T) {
			c := DefaultConfiguration()
			fields := map[string]*string{
				"repo_listen": &c.RepoListen, "web_listen": &c.WebListen, "repo_url": &c.RepoURL,
				"web_url": &c.WebURL, "upstream": &c.Upstream, "trusted_proxy": &c.TrustedProxy,
				"push_contact": &c.PushContact,
			}
			*fields[tc.field] = tc.value
			_, err := c.Options("")
			if err == nil || !strings.HasPrefix(err.Error(), tc.field+": ") {
				t.Errorf("expected rejection for %s, got %v", tc.field, err)
			}
		})
	}
}

func TestNetworkConfigurationNormalizesAddresses(t *testing.T) {
	c := DefaultConfiguration()
	c.RepoListen = "00080"
	c.WebListen = "[fe80:0::1%em0]:8181"
	c.RepoURL = "https://BÜCHER.example:444/FreeBSD:15:amd64/latest"
	c.WebURL = "https://[2001:db8::5]:8181/path%20prefix/"
	c.Upstream = "https://bücher.example./packages"
	c.TrustedProxy = "192.0.2.0/24"
	c.PushContact = "mailto:admin+pkg@bücher.example"
	o, err := c.Options("")
	if err != nil {
		t.Fatal(err)
	}
	if o.RepoListen != "[::]:80" || o.WebListen != "[fe80::1%em0]:8181" || o.PublicURL != "https://xn--bcher-kva.example:444/FreeBSD:15:amd64/latest" || o.WebsiteURL != c.WebURL || o.Upstream != "https://xn--bcher-kva.example./packages" || o.PushContact != "admin+pkg@xn--bcher-kva.example" {
		t.Errorf("unexpected addresses: %q, %q, %q, %q, %q, %q", o.RepoListen, o.WebListen, o.PublicURL, o.WebsiteURL, o.Upstream, o.PushContact)
	}
	if !reflect.DeepEqual(o.ConfigSettings, c) {
		t.Fatal("runtime normalization changed the editable settings")
	}
	for _, contact := range []string{"admin@localhost", "https://contact.example/support?id=4", "admin@[192.0.2.1]", "admin@[IPv6:2001:db8::1]"} {
		subscriber, err := pushSubscriber(contact)
		decoded, _ := url.PathUnescape(subscriber)
		if err != nil || contact != decoded {
			t.Errorf("contact %q: subscriber=%q error=%v", contact, subscriber, err)
		}
	}
}

func TestListenAddressConflicts(t *testing.T) {
	for _, tc := range []struct {
		repo, site string
		conflict   bool
	}{
		{"8080", "[::1]:8080", true},
		{"[::]:8080", "0.0.0.0:8080", false},
		{":8080", "[::1]:8080", true},
		{"0.0.0.0:8080", "127.0.0.1:08080", true},
		{"127.0.0.1:8080", "127.0.0.2:8080", false},
		{"[::ffff:127.0.0.1]:8080", "127.0.0.1:8080", true},
		{"[2001:db8:0::a]:81", "[2001:db8::a]:081", true},
		{"DÉMO.example:80", "xn--dmo-bma.example:80", true},
		{"localhost:80", "LOCALHOST:80", true},
		{"0", "[::]:0", false},
	} {
		c := DefaultConfiguration()
		c.RepoListen, c.WebListen = tc.repo, tc.site
		_, err := c.Options("")
		if (err != nil) != tc.conflict || err != nil && !strings.HasPrefix(err.Error(), "web_listen: overlaps") {
			t.Errorf("%s vs %s: %v", tc.repo, tc.site, err)
		}
	}
}
