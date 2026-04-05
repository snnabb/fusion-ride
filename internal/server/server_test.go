package server

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/snnabb/fusion-ride/internal/config"
)

func TestHandleHealthReturnsExpectedJSON(t *testing.T) {
	srv := &Server{cfg: config.Default()}

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	srv.handleHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var payload struct {
		Status    string `json:"status"`
		Upstreams struct {
			Online int `json:"online"`
			Total  int `json:"total"`
		} `json:"upstreams"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal health response failed: %v", err)
	}
	if payload.Status != "ok" {
		t.Fatalf("expected status ok, got %q", payload.Status)
	}
	if payload.Upstreams.Online != 0 || payload.Upstreams.Total != 0 {
		t.Fatalf("expected empty upstream counts, got %+v", payload.Upstreams)
	}
}

func TestResponseWriterHijackDelegatesToUnderlyingWriter(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	underlying := &hijackableResponseWriter{conn: serverConn}
	wrapped := &responseWriter{ResponseWriter: underlying, statusCode: http.StatusOK}

	conn, rw, err := wrapped.Hijack()
	if err != nil {
		t.Fatalf("expected Hijack to succeed: %v", err)
	}
	if conn == nil || rw == nil {
		t.Fatal("expected Hijack to return connection and buffered reader/writer")
	}
}

type hijackableResponseWriter struct {
	conn net.Conn
}

func (w *hijackableResponseWriter) Header() http.Header {
	return make(http.Header)
}

func (w *hijackableResponseWriter) Write(data []byte) (int, error) {
	return len(data), nil
}

func (w *hijackableResponseWriter) WriteHeader(statusCode int) {}

func (w *hijackableResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn)), nil
}
