package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/sevenofnine/proton-calendar-bridge/internal/app"
	"github.com/sevenofnine/proton-calendar-bridge/internal/config"
	"github.com/sevenofnine/proton-calendar-bridge/internal/tray"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level(cfg.LogLevel)}))
	result, err := app.BuildProviderWithAuth(cfg)
	if err != nil {
		return err
	}
	tr := tray.New("Proton Calendar Bridge", nil)
	application := app.New(cfg, result.Provider, tr, logger)
	application.SetAuthenticator(result.Authenticator)
	return application.Run(ctx)
}

func level(v string) slog.Level {
	switch v {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
