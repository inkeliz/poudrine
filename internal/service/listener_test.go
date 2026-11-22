package service

import (
	"net"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/config"
)

func TestHTTPListenerFamilies(t *testing.T) {
	o, err := config.DefaultConfiguration().Options("")
	if err != nil {
		t.Fatal(err)
	}
	if o.RepoListen != "[::]:8080" || o.WebListen != "[::]:8181" {
		t.Fatalf("unexpected defaults: repo %s, web %s", o.RepoListen, o.WebListen)
	}
	for _, tc := range []struct {
		address, allow, reject string
	}{
		{"[::]:0", "::1", "127.0.0.1"},
		{"0.0.0.0:0", "127.0.0.1", "::1"},
	} {
		t.Run(tc.address, func(t *testing.T) {
			l, err := listenHTTP(tc.address)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			_, port, err := net.SplitHostPort(l.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			conn, err := net.DialTimeout("tcp", net.JoinHostPort(tc.allow, port), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			conn.Close()
			if conn, err := net.DialTimeout("tcp", net.JoinHostPort(tc.reject, port), time.Second); err == nil {
				conn.Close()
				t.Fatalf("%s accepted a connection from %s", tc.address, tc.reject)
			}
		})
	}
}
