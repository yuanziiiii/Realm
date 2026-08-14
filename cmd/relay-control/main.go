package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"relaypanel/internal/control"
	"relaypanel/internal/store"
)

// version is replaced by release builds through -ldflags "-X main.version=...".
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print controller version")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	dbPath := env("RELAY_DB_PATH", "relay-panel.db")
	st, err := store.Open(dbPath)
	if err != nil {
		logger.Error("open database", "error", err)
		os.Exit(1)
	}
	defer st.Close()
	srv, err := control.New(context.Background(), st, control.Options{AdminPassword: os.Getenv("RELAY_ADMIN_PASSWORD"), SessionSecret: os.Getenv("RELAY_SESSION_SECRET"), SecureCookies: os.Getenv("RELAY_SECURE_COOKIES") == "true", WebURL: os.Getenv("RELAY_WEB_URL"), Logger: logger})
	if err != nil {
		logger.Error("initialize controller", "error", err)
		os.Exit(1)
	}
	httpServer := &http.Server{Addr: env("RELAY_LISTEN", ":8080"), Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 90 * time.Second}
	go func() {
		logger.Info("controller started", "listen", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("serve", "error", err)
			os.Exit(1)
		}
	}()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdown)
}
func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
