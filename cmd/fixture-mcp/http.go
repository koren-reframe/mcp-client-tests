package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// runHTTP serves either the streamable HTTP transport (POST /mcp) or the legacy
// HTTP+SSE transport (GET /sse plus POST /messages), and in both cases the
// OAuth discovery documents, so that the 401 modes lead somewhere real.
func runHTTP(s *Session, addr, portFile string, legacySSE bool) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fixture-mcp: listen: %v\n", err)
		os.Exit(2)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if portFile != "" {
		os.WriteFile(portFile, []byte(fmt.Sprintf("%d", port)), 0o644)
	}
	// Tests wait for this line rather than guessing a port or sleeping.
	fmt.Fprintf(os.Stderr, "fixture-mcp: listening port=%d transport=%s mode=%s\n", port,
		map[bool]string{true: "sse", false: "http"}[legacySSE], s.mode.Desc)

	outbound := make(chan []byte, 32) // server -> client frames
	var reqCount int64

	mux := http.NewServeMux()

	// --- OAuth discovery ----------------------------------------------------
	// Served unconditionally: the point of the 401 modes is to find out whether
	// the client goes looking for these, so they must exist to be found.
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		s.recordHTTP(r)
		writeJSON(w, 200, map[string]any{
			"resource":              fmt.Sprintf("http://127.0.0.1:%d/mcp", port),
			"authorization_servers": []string{fmt.Sprintf("http://127.0.0.1:%d", port)},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		s.recordHTTP(r)
		base := fmt.Sprintf("http://127.0.0.1:%d", port)
		writeJSON(w, 200, map[string]any{
			"issuer":                                base,
			"authorization_endpoint":                base + "/authorize",
			"token_endpoint":                        base + "/token",
			"registration_endpoint":                 base + "/register",
			"response_types_supported":              []string{"code"},
			"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported":      []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"none", "client_secret_post"},
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		s.recordHTTP(r)
		writeJSON(w, 201, map[string]any{
			"client_id":                  "fixture-client-id",
			"client_id_issued_at":        time.Now().Unix(),
			"redirect_uris":              []string{"http://127.0.0.1:0/callback"},
			"token_endpoint_auth_method": "none",
		})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		s.recordHTTP(r) // records code_challenge, state, redirect_uri for the PKCE assertions
		http.Error(w, "fixture authorization endpoint: consent not automated", http.StatusNotImplemented)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		s.recordHTTP(r)
		writeJSON(w, 200, map[string]any{
			"access_token": "fixture-access-token", "token_type": "Bearer", "expires_in": 3600,
		})
	})

	// --- streamable HTTP ----------------------------------------------------
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		s.recordHTTP(r)
		switch r.Method {
		case http.MethodGet:
			streamSSE(w, r, s, outbound, "")
		case http.MethodDelete:
			w.WriteHeader(204)
		case http.MethodPost:
			n := atomic.AddInt64(&reqCount, 1)
			if s.mode.ExpireSessionAfter > 0 && int(n) > s.mode.ExpireSessionAfter {
				// The client is expected to notice and re-initialize.
				writeJSON(w, 404, map[string]any{"error": "session not found"})
				return
			}
			s.servePost(w, r, outbound)
		default:
			w.WriteHeader(405)
		}
	})

	// --- legacy HTTP + SSE --------------------------------------------------
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		s.recordHTTP(r)
		// The legacy transport announces the POST endpoint as the first event.
		streamSSE(w, r, s, outbound, fmt.Sprintf("/messages?sessionId=fixture-%d", port))
	})
	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		s.recordHTTP(r)
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		// In the legacy transport the POST is only acknowledged; the reply
		// travels back over the SSE stream.
		body := readBody(r)
		act := s.Handle(body)
		for _, o := range act.Outs {
			if o.Delay > 0 {
				time.Sleep(o.Delay)
			}
			select {
			case outbound <- o.Bytes(s.rec):
			default:
			}
		}
		w.WriteHeader(202)
		if act.Exit != nil {
			s.rec.Note("exit", map[string]any{"code": *act.Exit})
			os.Exit(*act.Exit)
		}
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.recordHTTP(r)
		http.NotFound(w, r)
	})

	srv := &http.Server{Handler: mux}
	if err := srv.Serve(ln); err != nil {
		fmt.Fprintf(os.Stderr, "fixture-mcp: serve: %v\n", err)
	}
}

// servePost handles one JSON-RPC message posted to the streamable endpoint.
func (s *Session) servePost(w http.ResponseWriter, r *http.Request, outbound chan []byte) {
	body := readBody(r)

	var probe struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
	}
	json.Unmarshal(body, &probe)

	// Transport-level faults are decided before the protocol is consulted.
	if probe.Method == "initialize" && s.mode.HTTPStatusOnInit != 0 {
		if s.mode.HTTPAuthChallenge != "" {
			w.Header().Set("WWW-Authenticate", s.mode.HTTPAuthChallenge)
		}
		s.rec.Note("http_fault", map[string]any{"status": s.mode.HTTPStatusOnInit, "on": "initialize"})
		writeJSON(w, s.mode.HTTPStatusOnInit, map[string]any{"error": "fixture refused on purpose"})
		return
	}
	if s.mode.HTTPStall {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush() // headers out, body never arrives
		}
		s.rec.Note("http_stall", nil)
		time.Sleep(10 * time.Minute)
		return
	}

	act := s.Handle(body)
	if act.Hang {
		s.rec.Note("hang", map[string]any{"after": probe.Method})
		time.Sleep(10 * time.Minute)
		return
	}

	// A notification has no id, so there is nothing to answer: acknowledge it
	// and push anything the fixture wants to say over the SSE channel instead.
	if probe.ID == nil {
		for _, o := range act.Outs {
			select {
			case outbound <- o.Bytes(s.rec):
			default:
			}
		}
		w.WriteHeader(202)
		return
	}

	if s.mode.SSECloseMid {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"resu")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		s.rec.Note("sse_close_mid", nil)
		return // handler returns, connection closes mid-frame
	}

	if len(act.Outs) == 0 {
		w.WriteHeader(202)
		return
	}
	o := act.Outs[0]
	if o.Delay > 0 {
		time.Sleep(o.Delay)
	}
	b := o.Bytes(s.rec)
	if probe.Method == "initialize" {
		w.Header().Set("Mcp-Session-Id", fmt.Sprintf("fixture-session-%d", time.Now().UnixNano()))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	w.Write(b)

	for _, extra := range act.Outs[1:] { // stream faults, probes
		select {
		case outbound <- extra.Bytes(s.rec):
		default:
		}
	}
	if act.Exit != nil {
		s.rec.Note("exit", map[string]any{"code": *act.Exit})
		os.Exit(*act.Exit)
	}
}

// streamSSE holds an event stream open. endpointEvent is non-empty only for the
// legacy transport, which must announce its POST endpoint first.
func streamSSE(w http.ResponseWriter, r *http.Request, s *Session, outbound chan []byte, endpointEvent string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)
	f, _ := w.(http.Flusher)
	flush := func() {
		if f != nil {
			f.Flush()
		}
	}

	if endpointEvent != "" {
		fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", endpointEvent)
		flush()
		s.rec.Note("sse_endpoint_announced", map[string]any{"endpoint": endpointEvent})
	}

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case b := <-outbound:
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
			flush()
		case <-ticker.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flush()
		}
	}
}

func readBody(r *http.Request) []byte {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return buf
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// recordHTTP puts the request envelope in the transcript. Credential-bearing
// headers are recorded as a scheme plus a short hash of the value, never in
// clear: a test can still prove the right token was sent by comparing hashes,
// and the artifact stays safe to attach to a CI run.
func (s *Session) recordHTTP(r *http.Request) {
	headers := map[string]any{}
	for name, vals := range r.Header {
		v := strings.Join(vals, ", ")
		if isSensitiveHeader(name) {
			headers[strings.ToLower(name)] = redact(v)
			continue
		}
		headers[strings.ToLower(name)] = v
	}
	s.rec.Note("http_request", map[string]any{
		"method":  r.Method,
		"path":    r.URL.Path,
		"query":   r.URL.RawQuery,
		"headers": headers,
	})
}

func isSensitiveHeader(name string) bool {
	switch strings.ToLower(name) {
	case "authorization", "proxy-authorization", "x-api-key", "cookie":
		return true
	}
	return false
}

// redact keeps the scheme (so "Bearer" vs "Basic" is assertable) and replaces
// the credential with a truncated SHA-256 of it.
func redact(v string) map[string]any {
	scheme, cred := "", v
	if i := strings.IndexByte(v, ' '); i > 0 {
		scheme, cred = v[:i], strings.TrimSpace(v[i+1:])
	}
	sum := sha256.Sum256([]byte(cred))
	return map[string]any{
		"present":   true,
		"scheme":    scheme,
		"len":       len(cred),
		"sha256_12": hex.EncodeToString(sum[:])[:12],
	}
}
