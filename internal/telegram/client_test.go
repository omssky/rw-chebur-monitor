package telegram

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/omssky/rw-chebur-monitor/internal/monitor"
	"github.com/stretchr/testify/require"
)

type recordedRequest struct {
	path string
	form url.Values
}

func telegramServer(t *testing.T, status int, response string) (*Client, <-chan recordedRequest) {
	t.Helper()
	requests := make(chan recordedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
		}
		requests <- recordedRequest{r.URL.Path, r.Form}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, response)
	}))
	t.Cleanup(server.Close)
	b, err := bot.New("123:fake", bot.WithSkipGetMe(), bot.WithServerURL(server.URL), bot.WithHTTPClient(time.Second, server.Client()))
	require.NoError(t, err)
	return &Client{bot: b, chatID: -100123, threadID: 42}, requests
}

func TestSendMethodsAndTopic(t *testing.T) {
	for _, tc := range []struct {
		name         string
		notification monitor.Notification
		method       string
		resultID     int
	}{
		{"legacy", monitor.Notification{Body: "old queued message"}, "sendMessage", 73},
		{"new card", monitor.Notification{Event: &monitor.Event{Kind: monitor.EventCard, Card: sampleCard()}}, "sendRichMessage", 73},
		{"edit card", monitor.Notification{Event: &monitor.Event{Kind: monitor.EventCard, Card: sampleCard()}, MessageID: 41}, "editMessageText", 41},
		{"closing reply", monitor.Notification{Event: &monitor.Event{Kind: monitor.EventSummary, Card: sampleCard()}, MessageID: 41}, "sendRichMessage", 73},
		{"escalation reply", monitor.Notification{Event: &monitor.Event{Kind: monitor.EventEscalation, Card: sampleCard()}, MessageID: 41}, "sendRichMessage", 73},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, requests := telegramServer(t, http.StatusOK, `{"ok":true,"result":{"message_id":73,"date":1,"chat":{"id":-100123,"type":"supergroup"}}}`)
			id, err := client.Send(t.Context(), tc.notification)
			require.NoError(t, err)
			require.Equal(t, tc.resultID, id)
			request := <-requests
			require.Equal(t, "/bot123:fake/"+tc.method, request.path)
			require.Equal(t, "-100123", request.form.Get("chat_id"))
			if tc.method == "editMessageText" {
				require.Equal(t, "41", request.form.Get("message_id"))
				require.Empty(t, request.form.Get("message_thread_id"))
			} else {
				require.Equal(t, "42", request.form.Get("message_thread_id"))
			}
			if tc.notification.Event == nil {
				require.Equal(t, "old queued message", request.form.Get("text"))
				require.Empty(t, request.form.Get("rich_message"))
				return
			}
			var rich models.InputRichMessage
			require.NoError(t, json.Unmarshal([]byte(request.form.Get("rich_message")), &rich))
			require.Equal(t, render(*tc.notification.Event), rich.HTML)
			require.True(t, rich.SkipEntityDetection)
			if tc.notification.Event.Kind != monitor.EventCard {
				var reply models.ReplyParameters
				require.NoError(t, json.Unmarshal([]byte(request.form.Get("reply_parameters")), &reply))
				require.Equal(t, 41, reply.MessageID)
				require.True(t, reply.AllowSendingWithoutReply)
			} else {
				require.Empty(t, request.form.Get("reply_parameters"))
			}
		})
	}
}

func TestEditErrors(t *testing.T) {
	for _, tc := range []struct {
		description string
		unchanged   bool
		missing     bool
	}{
		{"Bad Request: message is not modified: specified new message content and reply markup are exactly the same", true, false},
		{"Bad Request: message is not modified", true, false},
		{"Bad Request: message to edit not found", false, true},
		{"Bad Request: message can't be edited", false, false},
		{"Bad Request: can't parse entities: message is not modified", false, false},
	} {
		t.Run(tc.description, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"ok": false, "error_code": 400, "description": tc.description})
			require.NoError(t, err)
			client, _ := telegramServer(t, http.StatusBadRequest, string(body))
			id, err := client.Send(t.Context(), monitor.Notification{Event: &monitor.Event{Kind: monitor.EventCard}, MessageID: 41})
			if tc.unchanged {
				require.NoError(t, err)
				require.Equal(t, 41, id)
				return
			}
			require.ErrorIs(t, err, bot.ErrorBadRequest)
			if tc.missing {
				var missing *monitor.MessageMissingError
				require.ErrorAs(t, err, &missing)
			}
		})
	}
}

func TestRateLimitPreservedForEveryMethod(t *testing.T) {
	for _, notification := range []monitor.Notification{
		{Body: "legacy"},
		{Event: &monitor.Event{Kind: monitor.EventCard}},
		{Event: &monitor.Event{Kind: monitor.EventCard}, MessageID: 41},
	} {
		client, _ := telegramServer(t, http.StatusTooManyRequests, `{"ok":false,"error_code":429,"description":"retry","parameters":{"retry_after":90}}`)
		_, err := client.Send(t.Context(), notification)
		var rate *monitor.RateLimitError
		require.ErrorAs(t, err, &rate)
		require.Equal(t, 90*time.Second, rate.After)
	}
}
