// SPDX-License-Identifier: Apache-2.0

package php

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// DefaultEtc is where Ubuntu keeps each PHP version's config (/etc/php/8.3/fpm/…).
const DefaultEtc = "/etc/php"

// StatusPath is the pm.status_path liops sets on pools that have none. It's only
// reachable through the pool's socket: nginx passes only .php scripts to PHP-FPM.
const StatusPath = "/liops-fpm-status"

// OPcacheScript is the read-only script liops installs to read OPcache's numbers; it
// runs in a pool through FastCGI, never through the web server. Tests point it elsewhere.
var OPcacheScript = "/usr/local/lib/liops/opcache-status.php"

// OPcacheScriptSource prints OPcache's numbers as JSON: sizes and counters, no file
// names or values.
const OPcacheScriptSource = `<?php
// Written by liops: OPcache's numbers for monitoring. It prints sizes and counters only.
header('Content-Type: application/json');
$s = function_exists('opcache_get_status') ? opcache_get_status(false) : false;
$c = function_exists('opcache_get_configuration') ? opcache_get_configuration() : false;
echo json_encode([
    'php' => PHP_VERSION,
    'enabled' => (bool) ($s['opcache_enabled'] ?? false),
    'memory' => $s['memory_usage'] ?? null,
    'strings' => $s['interned_strings_usage'] ?? null,
    'stats' => $s['opcache_statistics'] ?? null,
    'full' => (bool) ($s['cache_full'] ?? false),
    'max_files' => $c['directives']['opcache.max_accelerated_files'] ?? null,
    'memory_mb' => isset($c['directives']['opcache.memory_consumption']) ? $c['directives']['opcache.memory_consumption'] / 1048576 : null,
    'strings_mb' => $c['directives']['opcache.interned_strings_buffer'] ?? null,
    'validate_timestamps' => $c['directives']['opcache.validate_timestamps'] ?? null,
]);
`

// Pool is one PHP-FPM pool.
type Pool struct {
	Version     string // "8.3"
	Name        string // "www"
	File        string // its config file
	Listen      string // socket path, or host:port
	User        string
	PM          string // dynamic, static, ondemand
	MaxChildren int
	MaxRequests int    // pm.max_requests: 0 means workers are never recycled
	StatusPath  string // pm.status_path; empty when it's off
	SlowTimeout string // request_slowlog_timeout; empty or "0" when off
	SlowLog     string
}

// Label names the pool in series and the UI: "8.3/www".
func (p Pool) Label() string { return p.Version + "/" + p.Name }

// Service is the pool's systemd unit: php8.3-fpm.
func (p Pool) Service() string { return "php" + p.Version + "-fpm" }

// SlowLogOn reports whether the pool writes slow requests' stack traces.
func (p Pool) SlowLogOn() bool {
	return p.SlowTimeout != "" && strings.TrimRight(p.SlowTimeout, "s") != "0" && p.SlowLog != ""
}

// FindPools reads every PHP version's pool configs under etc (/etc/php/*/fpm/pool.d).
func FindPools(etc string) ([]Pool, error) {
	files, _ := filepath.Glob(filepath.Join(etc, "*", "fpm", "pool.d", "*.conf"))
	sort.Strings(files)
	var pools []Pool
	for _, f := range files {
		version := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(f))))
		ps, err := parsePoolFile(f, version)
		if err != nil {
			return pools, err
		}
		pools = append(pools, ps...)
	}
	return pools, nil
}

func parsePoolFile(path, version string) ([]Pool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var pools []Pool
	var cur *Pool
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == ';' || line[0] == '#' {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			pools = append(pools, Pool{Version: version, Name: line[1 : len(line)-1], File: path})
			cur = &pools[len(pools)-1]
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || cur == nil {
			continue
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
		v = strings.ReplaceAll(v, "$pool", cur.Name)
		switch k {
		case "listen":
			cur.Listen = v
		case "user":
			cur.User = v
		case "pm":
			cur.PM = v
		case "pm.max_children":
			cur.MaxChildren, _ = strconv.Atoi(v)
		case "pm.max_requests":
			cur.MaxRequests, _ = strconv.Atoi(v)
		case "pm.status_path":
			cur.StatusPath = v
		case "request_slowlog_timeout":
			cur.SlowTimeout = v
		case "slowlog":
			cur.SlowLog = v
		}
	}
	return pools, sc.Err()
}

// Status is a pool's status page (PHP-FPM's ?json).
type Status struct {
	ListenQueue    float64 `json:"listen queue"`
	MaxListenQueue float64 `json:"max listen queue"`
	ListenQueueLen float64 `json:"listen queue len"`
	Idle           float64 `json:"idle processes"`
	Active         float64 `json:"active processes"`
	Total          float64 `json:"total processes"`
	MaxActive      float64 `json:"max active processes"`
	MaxChildrenHit float64 `json:"max children reached"`
	SlowRequests   float64 `json:"slow requests"`
	AcceptedConn   float64 `json:"accepted conn"`
	StartSince     float64 `json:"start since"`
}

// ReadStatus asks a pool for its status page.
func ReadStatus(ctx context.Context, p Pool) (Status, error) {
	var st Status
	if p.StatusPath == "" {
		return st, fmt.Errorf("pool %s has no pm.status_path", p.Label())
	}
	res, err := Request(ctx, p.Listen, map[string]string{
		"SCRIPT_NAME": p.StatusPath, "SCRIPT_FILENAME": p.StatusPath, "REQUEST_URI": p.StatusPath + "?json",
		"QUERY_STRING": "json", "REQUEST_METHOD": "GET", "SERVER_PROTOCOL": "HTTP/1.1", "GATEWAY_INTERFACE": "CGI/1.1",
	})
	if err != nil {
		return st, err
	}
	if res.Status != 200 {
		return st, fmt.Errorf("status page: %d %s", res.Status, strings.TrimSpace(string(res.Body)))
	}
	return st, json.Unmarshal(res.Body, &st)
}

// OPcache is one PHP version's OPcache, as OPcacheScript reports it.
type OPcache struct {
	PHP     string `json:"php"`
	Enabled bool   `json:"enabled"`
	Memory  *struct {
		Used   float64 `json:"used_memory"`
		Free   float64 `json:"free_memory"`
		Wasted float64 `json:"wasted_memory"`
	} `json:"memory"`
	Strings *struct {
		Size float64 `json:"buffer_size"`
		Used float64 `json:"used_memory"`
	} `json:"strings"`
	Stats *struct {
		Scripts        float64 `json:"num_cached_scripts"`
		Keys           float64 `json:"num_cached_keys"`
		MaxKeys        float64 `json:"max_cached_keys"`
		Hits           float64 `json:"hits"`
		Misses         float64 `json:"misses"`
		OOMRestarts    float64 `json:"oom_restarts"`
		HashRestarts   float64 `json:"hash_restarts"`
		ManualRestarts float64 `json:"manual_restarts"`
	} `json:"stats"`
	Full      bool    `json:"full"`
	MaxFiles  float64 `json:"max_files"`
	MemoryMB  float64 `json:"memory_mb"`
	StringsMB float64 `json:"strings_mb"`
}

// ReadOPcache runs OPcacheScript in a pool.
func ReadOPcache(ctx context.Context, p Pool) (OPcache, error) {
	var o OPcache
	res, err := Request(ctx, p.Listen, map[string]string{
		"SCRIPT_NAME": "/opcache-status.php", "SCRIPT_FILENAME": OPcacheScript, "REQUEST_URI": "/opcache-status.php",
		"REQUEST_METHOD": "GET", "SERVER_PROTOCOL": "HTTP/1.1", "GATEWAY_INTERFACE": "CGI/1.1", "DOCUMENT_ROOT": filepath.Dir(OPcacheScript),
	})
	if err != nil {
		return o, err
	}
	if res.Status != 200 {
		return o, fmt.Errorf("OPcache script: %d %s %s", res.Status, strings.TrimSpace(string(res.Body)), res.Stderr)
	}
	return o, json.Unmarshal(res.Body, &o)
}
