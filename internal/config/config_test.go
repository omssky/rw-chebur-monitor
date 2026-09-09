package config

import (
	"strings"
	"testing"
	"time"

	"github.com/caarlos0/env/v11"
)

func TestEnvironmentAndDefaults(t *testing.T) {
	c, err := env.ParseAsWithOptions[Config](env.Options{Environment: map[string]string{
		"REMNAWAVE_URL": "https://panel.example.com", "REMNAWAVE_API_TOKEN": "panel-secret",
		"TELEGRAM_BOT_TOKEN": "123:fake", "TELEGRAM_CHAT_ID": "-100123", "TELEGRAM_THREAD_ID": "42",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.CheckInterval != 30*time.Minute || c.ConfirmDelay != 3*time.Minute {
		t.Fatal("wrong defaults")
	}
	if text := c.Redact("POST /bot123:fake/sendMessage: panel-secret"); strings.Contains(text, "123:fake") || strings.Contains(text, "panel-secret") {
		t.Fatal("secret leaked")
	}
	c.RemnawaveURL += "/api"
	if err := c.Validate(); err == nil {
		t.Fatal("ambiguous API prefix allowed")
	}
}
