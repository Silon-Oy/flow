package egresship

import (
	"errors"
	"testing"
	"time"
)

func TestParseLine(t *testing.T) {
	// Fixtures cover the four shapes the §11.6 shipper must handle correctly:
	// CONNECT tunnel allowed, CONNECT tunnel denied (= circumvention signal),
	// plain HTTP GET with full URL, and a denied non-CONNECT. Plus the noise
	// cases (blank, comment, malformed) that must not crash the tail loop.
	cases := []struct {
		name     string
		line     string
		wantSkip bool
		wantErr  bool
		host     string
		allowed  bool
		tsUnix   int64
	}{
		{
			name:    "connect tunnel allowed",
			line:    "1717250000.123    234 10.0.0.5 TCP_TUNNEL/200 1234 CONNECT github.com:443 - HIER_DIRECT/140.82.112.3 -",
			host:    "github.com",
			allowed: true,
			tsUnix:  1717250000,
		},
		{
			name:    "connect tunnel denied",
			line:    "1717250001.456      1 10.0.0.5 TCP_DENIED/403 4334 CONNECT evil.example.com:443 - HIER_NONE/- text/html",
			host:    "evil.example.com",
			allowed: false,
			tsUnix:  1717250001,
		},
		{
			name:    "plain http get",
			line:    "1717250002.789    42 10.0.0.5 TCP_MISS/200 8211 GET http://registry.npmjs.org/package - HIER_DIRECT/104.16.0.1 application/json",
			host:    "registry.npmjs.org",
			allowed: true,
			tsUnix:  1717250002,
		},
		{
			name:    "plain http denied",
			line:    "1717250003.000    10 10.0.0.5 TCP_MISS_DENIED/403 0 GET http://blocked.example.com/x - HIER_NONE/- -",
			host:    "blocked.example.com",
			allowed: false,
			tsUnix:  1717250003,
		},
		{
			name:    "https url with port and path",
			line:    "1717250004.000    10 10.0.0.5 TCP_MISS/200 100 GET https://api.github.com:443/repos/foo/bar - HIER_DIRECT/140.82.0.1 application/json",
			host:    "api.github.com",
			allowed: true,
			tsUnix:  1717250004,
		},
		{
			name:     "blank line",
			line:     "",
			wantSkip: true,
		},
		{
			name:     "comment line",
			line:     "# squid restart",
			wantSkip: true,
		},
		{
			name:    "truncated line",
			line:    "1717250005.0 12 10.0.0.5 TCP_MISS/200",
			wantErr: true,
		},
		{
			name:    "missing result code",
			line:    "1717250006.0 12 10.0.0.5 - 0 GET http://x/ - HIER_NONE/- -",
			wantErr: true,
		},
		{
			name:     "no target host skipped",
			line:     "1717250007.000 10 10.0.0.5 NONE/000 0 NONE - - HIER_NONE/- -",
			wantSkip: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseLine(tc.line)
			if tc.wantSkip {
				if !errors.Is(err, ErrSkip) {
					t.Fatalf("want ErrSkip, got entry=%+v err=%v", got, err)
				}
				return
			}
			if tc.wantErr {
				if err == nil || errors.Is(err, ErrSkip) {
					t.Fatalf("want error, got entry=%+v err=%v", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got.Host != tc.host {
				t.Errorf("host = %q, want %q", got.Host, tc.host)
			}
			if got.Allowed != tc.allowed {
				t.Errorf("allowed = %v, want %v", got.Allowed, tc.allowed)
			}
			if got.TS.Unix() != tc.tsUnix {
				t.Errorf("ts.Unix() = %d, want %d", got.TS.Unix(), tc.tsUnix)
			}
			if got.TS.Location() != time.UTC {
				t.Errorf("ts not UTC: %v", got.TS.Location())
			}
		})
	}
}
