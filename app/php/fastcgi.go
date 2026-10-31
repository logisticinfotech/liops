// SPDX-License-Identifier: Apache-2.0

// Package php reads PHP-FPM: its pools, their status pages and OPcache, through a
// minimal FastCGI client (the way nginx talks to PHP-FPM).
package php

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// FastCGI record types and the responder role (the FastCGI specification).
const (
	fcgiBeginRequest = 1
	fcgiEndRequest   = 3
	fcgiParams       = 4
	fcgiStdin        = 5
	fcgiStdout       = 6
	fcgiStderr       = 7
	fcgiResponder    = 1
)

// Response is a FastCGI response: the status, headers and body PHP wrote.
type Response struct {
	Status int
	Header textproto.MIMEHeader
	Body   []byte
	Stderr string
}

// listenAddr turns a pool's listen value into a network and address:
// "/run/php/php8.3-fpm.sock", "127.0.0.1:9000" or "9000".
func listenAddr(listen string) (string, string) {
	if strings.HasPrefix(listen, "/") {
		return "unix", listen
	}
	if _, err := strconv.Atoi(listen); err == nil {
		return "tcp", "127.0.0.1:" + listen
	}
	return "tcp", listen
}

// Request sends one FastCGI request with these parameters and no body.
func Request(ctx context.Context, listen string, params map[string]string) (Response, error) {
	network, addr := listenAddr(listen)
	var d net.Dialer
	conn, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(10 * time.Second)
	}
	conn.SetDeadline(deadline)

	var buf bytes.Buffer
	record := func(typ byte, content []byte) {
		h := []byte{1, typ, 0, 1, 0, 0, 0, 0} // version 1, request id 1
		binary.BigEndian.PutUint16(h[4:], uint16(len(content)))
		buf.Write(h)
		buf.Write(content)
	}
	record(fcgiBeginRequest, []byte{0, fcgiResponder, 0, 0, 0, 0, 0, 0})
	var p []byte
	for k, v := range params {
		p = appendLength(p, len(k))
		p = appendLength(p, len(v))
		p = append(p, k...)
		p = append(p, v...)
	}
	for len(p) > 0 {
		n := min(len(p), 65535)
		record(fcgiParams, p[:n])
		p = p[n:]
	}
	record(fcgiParams, nil)
	record(fcgiStdin, nil)
	if _, err := conn.Write(buf.Bytes()); err != nil {
		return Response{}, err
	}

	var stdout, stderr bytes.Buffer
	r := bufio.NewReader(conn)
	for {
		h := make([]byte, 8)
		if _, err := io.ReadFull(r, h); err != nil {
			return Response{}, fmt.Errorf("read FastCGI response: %w", err)
		}
		n, pad := int(binary.BigEndian.Uint16(h[4:])), int(h[6])
		content := make([]byte, n+pad)
		if _, err := io.ReadFull(r, content); err != nil {
			return Response{}, err
		}
		switch h[1] {
		case fcgiStdout:
			if stdout.Len()+n > 4<<20 {
				return Response{}, errors.New("FastCGI response too large")
			}
			stdout.Write(content[:n])
		case fcgiStderr:
			stderr.Write(content[:min(n, 4096)])
		case fcgiEndRequest:
			return parseResponse(stdout.Bytes(), stderr.String())
		}
	}
}

func appendLength(b []byte, n int) []byte {
	if n < 128 {
		return append(b, byte(n))
	}
	return binary.BigEndian.AppendUint32(b, uint32(n)|1<<31)
}

// parseResponse splits PHP's output into its CGI headers and body.
func parseResponse(out []byte, stderr string) (Response, error) {
	res := Response{Status: 200, Stderr: stderr}
	tp := textproto.NewReader(bufio.NewReader(bytes.NewReader(out)))
	h, err := tp.ReadMIMEHeader()
	if err != nil && !errors.Is(err, io.EOF) {
		return res, fmt.Errorf("FastCGI headers: %w", err)
	}
	res.Header = h
	if s := h.Get("Status"); s != "" {
		res.Status, _ = strconv.Atoi(strings.Fields(s)[0])
	}
	res.Body, _ = io.ReadAll(tp.R)
	return res, nil
}
