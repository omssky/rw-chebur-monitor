package telegram

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/omssky/rw-chebur-monitor/internal/monitor"
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
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{bot: b, chatID: -100123, threadID: 42}
	if err := client.Send(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	limited = true
	var limit *monitor.RateLimitError
	if err := client.Send(context.Background(), "test"); !errors.As(err, &limit) || limit.After != 90*time.Second {
		t.Fatal("retry_after not preserved", err)
	}
}
