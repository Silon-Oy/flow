// Package egresship parses the Flow egress proxy's Squid access log and ships
// host-level entries to flowd's POST /v1/egress endpoint (§11.6). It runs as a
// host-side goroutine inside the trusted flow-runner process — the untrusted
// per-run orchestrator containers never touch this path.
//
// The parser handles the default "squid" log format (deploy/egress-proxy/
// squid.conf line 25):
//
//	<ts> <elapsed> <client> <code>/<status> <bytes> <method> <url> <ident> <hier>/<peer> <mime>
//
// Only the timestamp, the result code, and the request target are forwarded —
// never bytes, query strings, credentials, or the client IP. `allowed` is true
// unless the Squid result code carries the `_DENIED` suffix (TCP_DENIED,
// TCP_MISS_DENIED, …).
package egresship

import (
	"errors"
	"net"
	"strconv"
	"strings"
	"time"
)

// Entry is a single parsed access.log line, ready for shipping.
type Entry struct {
	Host    string
	Allowed bool
	TS      time.Time
}

// ErrSkip signals that the line is intentionally ignored (blank, comment, or a
// log format the shipper does not need to surface — e.g. internal cache hits
// without a target host).
var ErrSkip = errors.New("egresship: skip line")

// ParseLine parses one Squid access.log line in the default "squid" format.
// Returns ErrSkip for blank/comment lines so the caller can keep tailing.
func ParseLine(line string) (Entry, error) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" || strings.HasPrefix(line, "#") {
		return Entry{}, ErrSkip
	}

	// Squid uses whitespace-separated fields; multiple spaces collapse, so a
	// plain Fields() suffices for the positional fields we need.
	f := strings.Fields(line)
	if len(f) < 7 {
		return Entry{}, errors.New("egresship: short line (<7 fields)")
	}

	tsSec, err := parseSquidTime(f[0])
	if err != nil {
		return Entry{}, err
	}

	code, _, ok := strings.Cut(f[3], "/")
	if !ok || code == "" {
		return Entry{}, errors.New("egresship: missing result code")
	}
	allowed := !strings.Contains(code, "_DENIED")

	host := extractHost(f[5], f[6])
	if host == "" {
		// No usable target host (e.g. internal cache management); skip.
		return Entry{}, ErrSkip
	}

	return Entry{Host: host, Allowed: allowed, TS: tsSec}, nil
}

// parseSquidTime accepts the "seconds.millis" epoch form Squid writes by
// default. UTC by definition.
func parseSquidTime(s string) (time.Time, error) {
	dot := strings.IndexByte(s, '.')
	var secStr, fracStr string
	if dot < 0 {
		secStr = s
	} else {
		secStr = s[:dot]
		fracStr = s[dot+1:]
	}
	sec, err := strconv.ParseInt(secStr, 10, 64)
	if err != nil {
		return time.Time{}, errors.New("egresship: bad timestamp seconds")
	}
	var nsec int64
	if fracStr != "" {
		// Pad/truncate to nanoseconds (Squid writes 3 digits by default).
		if len(fracStr) > 9 {
			fracStr = fracStr[:9]
		}
		for len(fracStr) < 9 {
			fracStr += "0"
		}
		n, err := strconv.ParseInt(fracStr, 10, 64)
		if err != nil {
			return time.Time{}, errors.New("egresship: bad timestamp fraction")
		}
		nsec = n
	}
	return time.Unix(sec, nsec).UTC(), nil
}

// extractHost pulls the destination hostname out of the method+url pair.
//
// CONNECT requests carry "host:port"; everything else carries a full URL. The
// goal is the host only — no path, port, scheme, or query.
func extractHost(method, target string) string {
	if target == "" || target == "-" {
		return ""
	}
	if strings.EqualFold(method, "CONNECT") {
		h, _, err := net.SplitHostPort(target)
		if err == nil {
			return h
		}
		return target
	}
	// Strip scheme if present.
	if i := strings.Index(target, "://"); i >= 0 {
		target = target[i+3:]
	}
	// Strip path/query.
	if i := strings.IndexAny(target, "/?#"); i >= 0 {
		target = target[:i]
	}
	// Strip user-info.
	if i := strings.LastIndexByte(target, '@'); i >= 0 {
		target = target[i+1:]
	}
	// Strip port.
	if h, _, err := net.SplitHostPort(target); err == nil {
		return h
	}
	return target
}
