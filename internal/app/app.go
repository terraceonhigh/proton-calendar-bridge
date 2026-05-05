package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/sevenofnine/proton-calendar-bridge/internal/api"
	authStore "github.com/sevenofnine/proton-calendar-bridge/internal/auth"
	"github.com/sevenofnine/proton-calendar-bridge/internal/config"
	"github.com/sevenofnine/proton-calendar-bridge/internal/protonapi"
	"github.com/sevenofnine/proton-calendar-bridge/internal/provider"
	"github.com/sevenofnine/proton-calendar-bridge/internal/security"
	"github.com/sevenofnine/proton-calendar-bridge/internal/tray"
)

type Application struct {
	cfg           config.Config
	provider      provider.CalendarProvider
	authenticator api.Authenticator
	tray          tray.App
	logger        *slog.Logger
}

func New(cfg config.Config, p provider.CalendarProvider, tr tray.App, logger *slog.Logger) *Application {
	if logger == nil {
		logger = slog.Default()
	}
	if tr == nil {
		tr = tray.NewNoop()
	}
	return &Application{cfg: cfg, provider: p, tray: tr, logger: logger}
}

func (a *Application) SetAuthenticator(auth api.Authenticator) {
	a.authenticator = auth
}

// BuildResult holds the provider and optional authenticator from BuildProvider.
type BuildResult struct {
	Provider      provider.CalendarProvider
	Authenticator api.Authenticator
}

func BuildProvider(cfg config.Config) (provider.CalendarProvider, error) {
	result, err := BuildProviderWithAuth(cfg)
	if err != nil {
		return nil, err
	}
	return result.Provider, nil
}

func BuildProviderWithAuth(cfg config.Config) (BuildResult, error) {
	providerType := strings.TrimSpace(cfg.ProviderType)
	if providerType == "" {
		providerType = strings.TrimSpace(cfg.Provider)
	}
	switch providerType {
	case "ics":
		return BuildResult{Provider: provider.NewICSProvider(cfg.ICSURL, nil)}, nil
	case "proton":
		client := protonapi.NewClient(protonapi.ClientOptions{})
		prov := provider.NewProtonProvider(client, authStore.Store{})
		authenticator := protonapi.NewAuthenticator(client)
		return BuildResult{Provider: prov, Authenticator: authenticator}, nil
	default:
		return BuildResult{}, fmt.Errorf("unsupported provider type: %s", providerType)
	}
}

func (a *Application) Run(ctx context.Context) error {
	server := api.New(api.Options{
		Provider: a.provider,
		Auth: security.BearerAuth{
			Enabled: a.cfg.RequireBearerToken,
			Token:   a.cfg.BearerToken,
		},
		Authenticator: a.authenticator,
		Logger:        a.logger,
	})

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 3)
	wg := sync.WaitGroup{}

	if a.cfg.BindAddress != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := server.ServeTCP(ctx, a.cfg.BindAddress); err != nil && !errors.Is(err, context.Canceled) {
				errCh <- fmt.Errorf("tcp server: %w", err)
			}
		}()
	}
	if a.cfg.UnixSocketPath != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := server.ServeUnix(ctx, a.cfg.UnixSocketPath); err != nil && !errors.Is(err, context.Canceled) {
				errCh <- fmt.Errorf("unix server: %w", err)
			}
		}()
	}

	if a.cfg.EnableTray {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := a.tray.Run(ctx); err != nil {
				errCh <- fmt.Errorf("tray: %w", err)
			}
		}()
	}

	select {
	case err := <-errCh:
		cancel()
		wg.Wait()
		return err
	case <-ctx.Done():
		wg.Wait()
		return nil
	}
}
