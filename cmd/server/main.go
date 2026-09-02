package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"buysms/internal/application"
	"buysms/internal/auth"
	"buysms/internal/config"
	"buysms/internal/httpapi"
	"buysms/internal/secure"
	"buysms/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("服务退出", "error", err)
		os.Exit(1)
	}
}
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	startup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repo, err := store.Open(startup, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer repo.Close()
	if err = repo.Migrate(startup); err != nil {
		return err
	}
	vault, err := secure.NewVault(cfg.EncryptionKey)
	if err != nil {
		return err
	}
	authentication := auth.New(repo, cfg.SessionPepper, cfg.AdminPath, cfg.CaptchaTTL, cfg.SessionTTL)
	app := application.New(repo, authentication, vault, cfg)
	if err = app.Bootstrap(startup); err != nil {
		return err
	}
	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go app.Run(rootCtx)
	server := &http.Server{Addr: cfg.Address, Handler: httpapi.New(app, authentication, cfg), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 40 * time.Second, WriteTimeout: 55 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	result := make(chan error, 1)
	go func() {
		if cfg.Environment == "production" {
			slog.Info("服务已启动", "address", cfg.Address)
		} else {
			slog.Info("服务已启动", "address", cfg.Address, "admin_path", cfg.AdminPath)
		}
		result <- server.ListenAndServe()
	}()
	select {
	case err = <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-rootCtx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		err = server.Shutdown(shutdown)
		if err != nil {
			_ = server.Close()
		}
		return err
	}
}
