// Command server runs the block52 per-table chat server: in-memory only,
// WebSocket transport, Cosmos-signed messages.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/block52/chat/internal/hub"
	"github.com/block52/chat/internal/ws"
)

func main() {
	// Self-healthcheck mode for the distroless image (which has no shell/curl):
	// `chat -healthcheck` hits /healthz on the local listen port and exits 0/1.
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(healthcheck(envStr("CHAT_LISTEN", ":8080")))
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	addr := envStr("CHAT_LISTEN", ":8080")
	expectedHRP := envStr("CHAT_EXPECTED_HRP", "") // e.g. "cosmos"; empty = any
	origins := splitCSV(envStr("CHAT_ALLOWED_ORIGINS", ""))

	h := hub.New(hub.Config{
		MaxMessagesPerTable: envInt("CHAT_MAX_MESSAGES", 200),
		MessageTTL:          time.Duration(envInt("CHAT_MESSAGE_TTL_SEC", 3600)) * time.Second,
		TableIdleTTL:        time.Duration(envInt("CHAT_TABLE_IDLE_TTL_SEC", 7200)) * time.Second,
		SweepInterval:       time.Duration(envInt("CHAT_SWEEP_SEC", 300)) * time.Second,
	})
	defer h.Close()

	mux := http.NewServeMux()
	mux.Handle("/ws", ws.Handler(ws.Options{
		Hub:             h,
		AllowedOrigins:  origins,
		ExpectedHRP:     expectedHRP,
		MaxMessageBytes: envInt("CHAT_MAX_MSG_BYTES", 4096),
		Logger:          logger,
	}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		tables, subs := h.Stats()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok","tables":` + strconv.Itoa(tables) +
			`,"subscribers":` + strconv.Itoa(subs) + `}`))
	})
	mux.Handle("/", http.FileServer(http.Dir("web")))

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("chat server listening", "addr", addr, "expectedHRP", expectedHRP)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

// healthcheck probes the local /healthz endpoint. The listen address may be
// ":8080"; rewrite a bare-host form to localhost for the client dial.
func healthcheck(listen string) int {
	host := listen
	if strings.HasPrefix(host, ":") {
		host = "127.0.0.1" + host
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + host + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
