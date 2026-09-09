package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/omssky/rw-chebur-monitor/internal/cheburcheck"
	"github.com/omssky/rw-chebur-monitor/internal/config"
	"github.com/omssky/rw-chebur-monitor/internal/monitor"
	"github.com/omssky/rw-chebur-monitor/internal/remnawave"
	"github.com/omssky/rw-chebur-monitor/internal/sqlite"
	"github.com/omssky/rw-chebur-monitor/internal/telegram"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	c, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %s", c.Redact(err.Error()))
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			if err, ok := attr.Value.Any().(error); ok {
				attr.Value = slog.StringValue(c.Redact(err.Error()))
			} else if attr.Value.Kind() == slog.KindString {
				attr.Value = slog.StringValue(c.Redact(attr.Value.String()))
			}
			return attr
		},
	}))
	store, err := sqlite.Open(c.DBPath)
	if err != nil {
		return err
	}
	defer store.Close()
	sender, err := telegram.New(c.TelegramToken, c.ChatID, c.ThreadID, httpClient(20*time.Second))
	if err != nil {
		return fmt.Errorf("Telegram: %s", c.Redact(err.Error()))
	}
	service := monitor.Service{
		Discovery: remnawave.New(c.RemnawaveURL, c.RemnawaveToken, httpClient(20*time.Second)),
		Checker:   cheburcheck.New("https://cheburcheck.ru", httpClient(0)),
		Sender:    sender,
		Store:     store,
		Policy:    monitor.Policy{Interval: c.CheckInterval, ConfirmDelay: c.ConfirmDelay},
		Log:       logger,
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	logger.Info("Starting monitor", "interval", c.CheckInterval)
	if err := service.Run(ctx); err != nil {
		return fmt.Errorf("monitor: %s", c.Redact(err.Error()))
	}
	return nil
}

func httpClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 20 * time.Second
	return &http.Client{
		Timeout: timeout, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
