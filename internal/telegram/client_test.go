package telegram

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/omssky/rw-chebur-monitor/internal/monitor"
	"github.com/stretchr/testify/require"
)

func TestTopicAndRateLimit(t *testing.T) {
	limited := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bot123:fake/sendMessage" {
			t.Error(r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
		}
		if r.FormValue("chat_id") != "-100123" || r.FormValue("message_thread_id") != "42" || r.FormValue("text") != "test" {
			t.Error("wrong destination", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		if limited {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"ok":false,"error_code":429,"description":"retry","parameters":{"retry_after":90}}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"date":1,"chat":{"id":-100123,"type":"supergroup"},"text":"test"}}`)
	}))
	defer server.Close()
	b, err := bot.New("123:fake", bot.WithSkipGetMe(), bot.WithServerURL(server.URL))
	require.NoError(t, err)
	client := &Client{bot: b, chatID: -100123, threadID: 42}
	require.NoError(t, client.Send(t.Context(), "test"))
	limited = true
	var limit *monitor.RateLimitError
	err = client.Send(t.Context(), "test")
	require.ErrorAs(t, err, &limit)
	require.Equal(t, 90*time.Second, limit.After)
}
