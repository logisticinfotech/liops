// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"encoding/json"
	"math"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// NginxLogPath is the access log liops adds to nginx, in its own JSON format (NginxLogFormat).
const NginxLogPath = "/var/log/nginx/liops-access.log"

// NginxLogFormat is the log_format liops adds to nginx: JSON (escape=json), so no URL or
// user agent can break a line, with the request's host and its times.
const NginxLogFormat = `log_format liops escape=json '{"t":$msec,"host":"$host","ip":"$remote_addr","m":"$request_method","u":"$request_uri","s":$status,"b":$body_bytes_sent,"rt":$request_time,"ut":"$upstream_response_time","ua":"$http_user_agent"}';`

// ApacheLogPath is the access log liops adds to Apache, in ApacheLogFormat.
const ApacheLogPath = "/var/log/apache2/liops-access.log"

// ApacheLogFormat is the LogFormat liops adds to Apache: tab-separated (Apache escapes
// tabs and quotes inside fields as \xhh, so a field can't break a line), with the time in
// milliseconds, the Host header, and the request time in microseconds (%D).
const ApacheLogFormat = `LogFormat "%{%s}t.%{msec_frac}t\t%{Host}i\t%a\t%m\t%U%q\t%>s\t%B\t%D\t%{User-Agent}i" liops`

// Request is one line of an access log.
type Request struct {
	At         time.Time
	Host       string
	Method     string
	Path       string // without the query string
	Status     int
	MS         float64
	UpstreamMS float64 // -1 when there's no upstream time (static files, Caddy)
	Bytes      int64
	IP         string
	UA         string
}

// parseNginx reads a line in NginxLogFormat.
func parseNginx(line []byte) (Request, bool) {
	var l struct {
		T    float64 `json:"t"`
		Host string  `json:"host"`
		IP   string  `json:"ip"`
		M    string  `json:"m"`
		U    string  `json:"u"`
		S    int     `json:"s"`
		B    int64   `json:"b"`
		RT   float64 `json:"rt"`
		UT   string  `json:"ut"`
		UA   string  `json:"ua"`
	}
	if json.Unmarshal(line, &l) != nil || l.S == 0 {
		return Request{}, false
	}
	r := Request{At: unixFloat(l.T), Host: l.Host, Method: l.M, Path: l.U, Status: l.S, MS: l.RT * 1000, UpstreamMS: -1, Bytes: l.B, IP: l.IP, UA: l.UA}
	// "0.012", "0.012, 0.031" (retried upstreams), or "-".
	for _, part := range strings.Split(l.UT, ",") {
		if v, err := strconv.ParseFloat(strings.TrimSpace(part), 64); err == nil {
			r.UpstreamMS = math.Max(r.UpstreamMS, 0) + v*1000
		}
	}
	return r, true
}

// parseApache reads a line in ApacheLogFormat. Apache has no separate upstream time.
func parseApache(line []byte) (Request, bool) {
	f := strings.Split(string(line), "\t")
	if len(f) != 9 {
		return Request{}, false
	}
	t, err1 := strconv.ParseFloat(f[0], 64)
	status, err2 := strconv.Atoi(f[5])
	us, err3 := strconv.ParseFloat(f[7], 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return Request{}, false
	}
	b, _ := strconv.ParseInt(f[6], 10, 64)
	return Request{At: unixFloat(t), Host: f[1], IP: f[2], Method: f[3], Path: f[4], Status: status, Bytes: b, MS: us / 1000, UpstreamMS: -1, UA: f[8]}, true
}

// parseCaddy reads a Caddy JSON access log line ("handled request").
func parseCaddy(line []byte) (Request, bool) {
	var l struct {
		Logger  string  `json:"logger"`
		TS      float64 `json:"ts"`
		Request struct {
			RemoteIP string              `json:"remote_ip"`
			ClientIP string              `json:"client_ip"`
			Method   string              `json:"method"`
			Host     string              `json:"host"`
			URI      string              `json:"uri"`
			Headers  map[string][]string `json:"headers"`
		} `json:"request"`
		Duration float64 `json:"duration"` // seconds
		Size     int64   `json:"size"`
		Status   int     `json:"status"`
	}
	if json.Unmarshal(line, &l) != nil || !strings.HasPrefix(l.Logger, "http.log.access") || l.Status == 0 {
		return Request{}, false
	}
	ip := l.Request.ClientIP
	if ip == "" {
		ip = l.Request.RemoteIP
	}
	var ua string
	if v := l.Request.Headers["User-Agent"]; len(v) > 0 {
		ua = v[0]
	}
	return Request{At: unixFloat(l.TS), Host: l.Request.Host, Method: l.Request.Method, Path: l.Request.URI, Status: l.Status,
		MS: l.Duration * 1000, UpstreamMS: -1, Bytes: l.Size, IP: ip, UA: ua}, true
}

func unixFloat(f float64) time.Time {
	sec, frac := math.Modf(f)
	return time.Unix(int64(sec), int64(frac*1e9))
}

var (
	uuidRE  = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	hexRE   = regexp.MustCompile(`^[0-9a-fA-F]{16,}$`)
	digitRE = regexp.MustCompile(`[0-9]`)
	alphaRE = regexp.MustCompile(`[A-Za-z]`)
)

var staticExt = map[string]bool{"css": true, "js": true, "mjs": true, "map": true, "png": true, "jpg": true, "jpeg": true, "gif": true,
	"svg": true, "webp": true, "avif": true, "ico": true, "woff": true, "woff2": true, "ttf": true, "eot": true, "mp4": true, "webm": true, "txt": true, "xml": true, "pdf": true}

// Route turns a path into its route, so similar requests add up: query strings are
// dropped, and numbers, UUIDs, long hex strings, tokens and e-mail addresses become
// placeholders. /orders/12345?page=2 → /orders/{n}; a static file → /build/assets/{file}.js.
func Route(path string) string {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	if path == "" || path[0] != '/' {
		return "/{other}" // CONNECT host:443, a bare * and the like
	}
	segs := strings.Split(path[1:], "/")
	if len(segs) > 6 {
		segs = append(segs[:6], "…")
	}
	last := len(segs) - 1
	for i, s := range segs {
		switch {
		case s == "" || s == "…":
		case i == last && strings.Contains(s, ".") && staticExt[strings.ToLower(s[strings.LastIndexByte(s, '.')+1:])]:
			segs[i] = "{file}." + strings.ToLower(s[strings.LastIndexByte(s, '.')+1:])
		case isDigits(s):
			segs[i] = "{n}"
		case uuidRE.MatchString(s):
			segs[i] = "{uuid}"
		case hexRE.MatchString(s):
			segs[i] = "{hex}"
		case strings.Contains(s, "@"):
			segs[i] = "{email}"
		case len(s) >= 20 && digitRE.MatchString(s) && alphaRE.MatchString(s), len(s) > 64:
			segs[i] = "{token}"
		}
	}
	return "/" + strings.Join(segs, "/")
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

// normalHost lowercases a host and drops its port.
func normalHost(h string) string {
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	if h == "" {
		return "-"
	}
	return h
}

// ClientClass sorts a user agent into what people would call it.
func ClientClass(ua string) string {
	l := strings.ToLower(ua)
	switch {
	case ua == "" || ua == "-":
		return "no user agent"
	case strings.Contains(l, "bot") || strings.Contains(l, "crawler") || strings.Contains(l, "spider") || strings.Contains(l, "slurp"):
		return "bot"
	case strings.HasPrefix(l, "curl/") || strings.HasPrefix(l, "wget/") || strings.Contains(l, "python") || strings.HasPrefix(l, "go-http-client") ||
		strings.Contains(l, "okhttp") || strings.Contains(l, "guzzle") || strings.HasPrefix(l, "java/") || strings.Contains(l, "axios") || strings.Contains(l, "node-fetch"):
		return "script or app"
	case strings.Contains(l, "mobile") || strings.Contains(l, "android") || strings.Contains(l, "iphone"):
		return "mobile browser"
	case strings.Contains(l, "mozilla"):
		return "browser"
	}
	return "other"
}
