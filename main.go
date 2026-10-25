package main

import (
	"context"
	"crypto/rand"
	"embed"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed web/*
var webFS embed.FS

func main() {
	port := getenv("PORT", "8080")
	reqTimeout := time.Duration(getenvInt("REQUEST_TIMEOUT_SECONDS", 120)) * time.Second
	streamTimeout := time.Duration(getenvInt("STREAM_TIMEOUT_SECONDS", 300)) * time.Second
	sessionTTL := time.Duration(getenvInt("SESSION_TTL_HOURS", 24)) * time.Hour

	master := os.Getenv("MASTER_PASSWORD")
	if master == "" {
		master = randomPassword()
	}

	st := newStore(master)
	srv := &server{
		store:         st,
		upstream:      newUpstream(),
		reqTimeout:    reqTimeout,
		streamTimeout: streamTimeout,
		sessionTTL:    sessionTTL,
	}
	srv.startSessionCleanup()

	mux := http.NewServeMux()
	srv.routes(mux)

	log.Printf("")
	log.Printf("==============================================================")
	log.Printf("  Completions API gateway")
	log.Printf("")
	log.Printf("  Admin dashboard:    http://localhost:%s/", port)
	log.Printf("  Completions API:    POST http://localhost:%s/v1/chat/completions", port)
	log.Printf("  Models list:        GET  http://localhost:%s/v1/models", port)
	log.Printf("  API docs (llms.txt): GET  http://localhost:%s/llms.txt", port)
	log.Printf("")
	log.Printf("  MASTER PASSWORD:    %s", master)
	log.Printf("  (shown only once at startup; store it somewhere safe)")
	log.Printf("==============================================================")
	log.Printf("")

	httpServer := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		log.Printf("shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	}()

	log.Printf("listening on %s", ":"+port)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}

func randomPassword() string {
	const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	parts := make([]string, 4)
	for i := range parts {
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		chars := make([]byte, 4)
		for j := range b {
			chars[j] = alphabet[int(b[j])%len(alphabet)]
		}
		parts[i] = string(chars)
	}
	return strings.Join(parts, "-")
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
