package httpx_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/yteraoka/kibitz/internal/httpx"
)

func TestServeShutsDownCleanly(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           http.NewServeMux(),
		ReadHeaderTimeout: time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- httpx.Serve(ctx, discardLogger(), "test", srv, 2*time.Second) }()

	waitForListener(t, addr)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil on clean shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the context was cancelled")
	}
}

func TestServeReportsListenFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	srv := &http.Server{
		Addr:              ln.Addr().String(), // already taken
		Handler:           http.NewServeMux(),
		ReadHeaderTimeout: time.Second,
	}

	err = httpx.Serve(context.Background(), discardLogger(), "test", srv, time.Second)
	if err == nil {
		t.Fatal("Serve returned nil, want the bind failure")
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		t.Fatalf("err = %v, want a net.OpError", err)
	}
}

func waitForListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server never started listening on %s", addr)
}
