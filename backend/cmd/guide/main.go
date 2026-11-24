package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/LYP-leo/echo-shopping-guide/backend/src/guide"
	"github.com/LYP-leo/echo-shopping-guide/backend/src/llm"
	"github.com/LYP-leo/echo-shopping-guide/backend/src/ticketing"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client := ticketing.NewFromEnv()
	defer client.Close()
	guideHandler, ticketingMux := guide.New(client, llm.ConfigFromEnv())
	mux := http.NewServeMux()
	mux.Handle("/api/v1/ticketing/", ticketingMux)
	// SSE 需要即时 Flush，单独挂载，不走 65 秒超时包装。
	streamMux := http.NewServeMux()
	streamMux.Handle("POST /api/v1/ticketing/assist/stream", guideHandler.StreamHandler())
	streamMux.Handle("POST /api/v1/ticketing/agent/stream", guideHandler.AgentStreamHandler())
	streamMux.Handle("POST /api/v1/ticketing/agent/resume/{run_id}/stream", guideHandler.AgentResumeStreamHandler())
	mux.Handle("/api/v1/guide-admin/", guideHandler.AdminHandler())
	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok","service":"echo-shopping-guide"}`))
	})
	executable, _ := os.Executable()
	webRoot := filepath.Join(filepath.Dir(executable), "..", "frontend", "dist")
	if _, err := os.Stat(webRoot); err != nil {
		webRoot = filepath.Join("..", "frontend", "dist")
	}
	files := http.FileServer(http.Dir(webRoot))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		clean := filepath.Clean(filepath.FromSlash("/" + r.URL.Path))
		path := filepath.Join(webRoot, strings.TrimLeft(clean, "/\\"))
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			files.ServeHTTP(w, r)
			return
		}
		if filepath.Ext(r.URL.Path) != "" {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(webRoot, "index.html"))
	})
	addr := strings.TrimSpace(os.Getenv("GUIDE_ADDR"))
	if addr == "" {
		addr = "127.0.0.1:8082"
	}
	wrapped := http.NewServeMux()
	wrapped.Handle("POST /api/v1/ticketing/assist/stream", streamMux)
	wrapped.Handle("POST /api/v1/ticketing/agent/stream", streamMux)
	wrapped.Handle("POST /api/v1/ticketing/agent/resume/{run_id}/stream", streamMux)
	wrapped.Handle("/", http.TimeoutHandler(mux, 65*time.Second, `{"message":"查询超时，请稍后重试"}`))
	server := &http.Server{Addr: addr, Handler: wrapped, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		slog.Info("Echo shopping guide listening", "addr", addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("guide stopped", "error", err)
			stop()
		}
	}()
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdown)
}
