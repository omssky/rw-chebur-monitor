package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

type Config struct {
	RemnawaveURL   string        `env:"REMNAWAVE_URL,required,notEmpty"`
	RemnawaveToken string        `env:"REMNAWAVE_API_TOKEN,required,notEmpty"`
	TelegramToken  string        `env:"TELEGRAM_BOT_TOKEN,required,notEmpty"`
	ChatID         int64         `env:"TELEGRAM_CHAT_ID,required"`
	ThreadID       int           `env:"TELEGRAM_THREAD_ID,required"`
	CheckInterval  time.Duration `env:"CHECK_INTERVAL" envDefault:"30m"`
	ConfirmDelay   time.Duration `env:"CONFIRM_DELAY" envDefault:"3m"`
	DBPath         string        `env:"DB_PATH" envDefault:"/data/monitor.db"`
}

func Load() (Config, error) {
	c, err := env.ParseAs[Config]()
	if err != nil {
		return c, err
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	u, err := url.Parse(c.RemnawaveURL)
	if err != nil || u.Hostname() == "" || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
		return fmt.Errorf("REMNAWAVE_URL must be an HTTPS base URL without /api, credentials or query")
	}
	if c.ChatID == 0 || c.ThreadID <= 0 {
		return fmt.Errorf("TELEGRAM_CHAT_ID must be nonzero and TELEGRAM_THREAD_ID positive")
	}
	if c.ConfirmDelay < 15*time.Second || c.CheckInterval <= c.ConfirmDelay {
		return fmt.Errorf("CONFIRM_DELAY must be at least 15s and shorter than CHECK_INTERVAL")
	}
	if c.DBPath == "" {
		return fmt.Errorf("DB_PATH cannot be empty")
	}
	return nil
}

func (c Config) Redact(message string) string {
	for _, secret := range []string{c.RemnawaveToken, c.TelegramToken} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	return message
}
