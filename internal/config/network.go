package config

import (
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/net/idna"
)

// Validate syntax locally so settings may be prepared before DNS or an interface is available.
var hostnameLabel = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

var numericHost = regexp.MustCompile(`(?i)^(0x[0-9a-f]*|[0-9]+)(\.(0x[0-9a-f]*|[0-9]+))*$`)

var interfaceZone = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func dnsHostname(host string) (string, error) {
	ascii := strings.ToLower(host)
	var err error
	if strings.Contains(ascii, "xn--") || strings.IndexFunc(host, func(r rune) bool { return r > unicode.MaxASCII }) >= 0 {
		ascii, err = idna.Lookup.ToASCII(host)
	}
	name := strings.TrimSuffix(ascii, ".")
	if err != nil || name == "" || len(name) > 253 || numericHost.MatchString(name) {
		return "", errors.New("invalid DNS hostname or IP address")
	}
	for label := range strings.SplitSeq(name, ".") {
		if !hostnameLabel.MatchString(label) {
			return "", errors.New("invalid DNS hostname label")
		}
	}
	return ascii, nil
}

func normalizeURLHost(u *url.URL) error {
	port := u.Port()
	if port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return errors.New("URL port must be between 1 and 65535")
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return errors.New("missing URL port")
	}
	host := u.Hostname()
	ip, err := netip.ParseAddr(host)
	if err == nil {
		if strings.HasPrefix(u.Host, "[") != ip.Is6() {
			return errors.New("enclose IPv6 addresses in brackets and use plain IPv4 addresses")
		}
		if ip.Unmap().IsUnspecified() || ip.Unmap().IsMulticast() {
			return errors.New("use a reachable unicast address or hostname")
		}
		if ip.Zone() != "" && !interfaceZone.MatchString(ip.Zone()) {
			return errors.New("invalid IPv6 interface zone")
		}
		return nil
	}
	if strings.HasPrefix(u.Host, "[") {
		return errors.New("expected an IPv6 address inside brackets")
	}
	host, err = dnsHostname(host)
	if err != nil {
		return err
	}
	u.Host = host
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	}
	return nil
}

func publicHTTPURL(raw string) (string, error) {
	return baseHTTPURL(raw, false)
}

func baseHTTPURL(raw string, forBrowser bool) (string, error) {
	if raw == "" {
		return "", nil
	}
	u, err := WebURL(raw)
	if err != nil || strings.ContainsAny(raw, "# ") {
		return "", errors.New("expected an absolute http:// or https:// base URL without credentials, query or fragment")
	}
	if err := normalizeURLHost(u); err != nil {
		return "", err
	}
	// Browser URL parsers cannot navigate to an IPv6 address containing a zone.
	if forBrowser && strings.Contains(u.Hostname(), "%") {
		return "", errors.New("use a website hostname or IPv6 address without an interface zone")
	}
	if strings.Contains(u.Path, "\\") || strings.IndexFunc(u.Path, unicode.IsControl) >= 0 {
		return "", errors.New("URL path contains a backslash or control character")
	}
	for segment := range strings.SplitSeq(u.Path, "/") {
		if segment == "." || segment == ".." {
			return "", errors.New("use a base URL without . or .. path segments")
		}
	}
	return u.String(), nil
}

func listenAddress(value string) (string, error) {
	// A port alone uses the IPv6-only wildcard. Port zero requests an ephemeral port.
	if !strings.Contains(value, ":") {
		value = net.JoinHostPort("::", value)
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return "", errors.New("expected a port or an address such as [::]:8080")
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return "", errors.New("expected a decimal TCP port between 0 and 65535")
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Unmap().IsMulticast() || ip.Zone() != "" && !interfaceZone.MatchString(ip.Zone()) {
			return "", errors.New("use a unicast or wildcard address with a valid interface zone")
		}
		host = ip.Unmap().String()
	} else if host != "" {
		if strings.HasPrefix(value, "[") {
			return "", errors.New("expected an IPv6 address inside brackets")
		}
		host, err = dnsHostname(host)
		if err != nil {
			return "", errors.New("expected an IP address or DNS hostname followed by a port")
		}
	} else if strings.HasPrefix(value, "[") {
		return "", errors.New("empty bracketed listen address")
	}
	return net.JoinHostPort(host, strconv.FormatUint(n, 10)), nil
}

// Both arguments have passed listenAddress. Hostname aliases are resolved at bind time.
func listenersOverlap(first, second string) bool {
	a, portA, _ := net.SplitHostPort(first)
	b, portB, _ := net.SplitHostPort(second)
	if portA == "0" || portA != portB {
		return false
	}
	if a == b || a == "" || b == "" {
		return true
	}
	ipA, errA := netip.ParseAddr(a)
	ipB, errB := netip.ParseAddr(b)
	return errA == nil && errB == nil && ipA.BitLen() == ipB.BitLen() && (ipA.IsUnspecified() || ipB.IsUnspecified())
}

func checkProxyCIDR(value string) error {
	if value == "" {
		return nil
	}
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		return errors.New("expected an IPv4 or IPv6 network in CIDR notation")
	}
	if prefix.Bits() == 0 || prefix.Addr().Is4In6() && prefix.Bits() <= 96 {
		return errors.New("trust a specific proxy address or network, not all client addresses")
	}
	if network := prefix.Masked(); network != prefix {
		return fmt.Errorf("use %s for the network or %s/%d for a single proxy", network, prefix.Addr(), prefix.Addr().BitLen())
	}
	return nil
}

// webpush-go accepts either an HTTPS URI or the mailbox portion of a mailto URI.
func pushSubscriber(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if strings.HasPrefix(strings.ToLower(value), "https://") {
		u, err := url.Parse(value)
		if err != nil || u.User != nil || strings.ContainsAny(value, " \\") || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return "", errors.New("enter an HTTPS contact URL without credentials or whitespace")
		}
		if err := normalizeURLHost(u); err != nil {
			return "", err
		}
		return u.String(), nil
	}
	email := value
	if strings.HasPrefix(strings.ToLower(value), "mailto:") {
		var err error
		email, err = url.PathUnescape(value[len("mailto:"):])
		if err != nil || strings.ContainsAny(value, "?#") {
			return "", errors.New("enter a mailto address without a query or fragment")
		}
	}
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email || strings.IndexFunc(email, unicode.IsControl) >= 0 {
		return "", errors.New("enter an email address, a mailto: address, or an HTTPS contact URL")
	}
	local, domain, _ := strings.Cut(email, "@")
	// mail.ParseAddress has already checked bracketed IP address literals.
	if !strings.HasPrefix(domain, "[") {
		domain, err = dnsHostname(domain)
		if err != nil {
			return "", errors.New("enter a valid email domain")
		}
	}
	return url.PathEscape(local + "@" + domain), nil
}
