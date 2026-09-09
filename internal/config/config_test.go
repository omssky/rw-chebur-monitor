package config

import (
	"testing"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/stretchr/testify/require"
)

func TestEnvironmentAndDefaults(t *testing.T) {
	c, err := env.ParseAsWithOptions[Config](env.Options{Environment: map[string]string{
		"REMNAWAVE_URL": "https://panel.example.com", "REMNAWAVE_API_TOKEN": "panel-secret",
		"TELEGRAM_BOT_TOKEN": "123:fake", "TELEGRAM_CHAT_ID": "-100123", "TELEGRAM_THREAD_ID": "42",
	}})
	require.NoError(t, err)
	require.NoError(t, c.Validate())
	require.Equal(t, 30*time.Minute, c.CheckInterval)
	require.Equal(t, 3*time.Minute, c.ConfirmDelay)
	require.Equal(t, "POST /bot[redacted]/sendMessage: [redacted]", c.Redact("POST /bot123:fake/sendMessage: panel-secret"))

	c.RemnawaveURL += "/api"
	require.Error(t, c.Validate(), "ambiguous API prefix must be rejected")
}
