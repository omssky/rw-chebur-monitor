package telegram

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/omssky/rw-chebur-monitor/internal/monitor"
)

type Client struct {
	bot      *bot.Bot
	chatID   int64
	threadID int
}

func New(token string, chatID int64, threadID int, client *http.Client) (*Client, error) {
	b, err := bot.New(token, bot.WithSkipGetMe(), bot.WithHTTPClient(20*time.Second, client))
	if err != nil {
		return nil, err
	}
	return &Client{bot: b, chatID: chatID, threadID: threadID}, nil
}

func (c *Client) Send(ctx context.Context, notification monitor.Notification) (int, error) {
	var message *models.Message
	var err error
	if notification.Event == nil {
		disabled := true
		message, err = c.bot.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: c.chatID, MessageThreadID: c.threadID, Text: notification.Body,
			LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: &disabled},
		})
	} else {
		rich := models.InputRichMessage{HTML: render(*notification.Event), SkipEntityDetection: true}
		if notification.Event.Kind == monitor.EventCard && notification.MessageID != 0 {
			_, err = c.bot.EditMessageText(ctx, &bot.EditMessageTextParams{
				ChatID: c.chatID, MessageID: notification.MessageID, RichMessage: &rich,
			})
			if errors.Is(err, bot.ErrorBadRequest) {
				description := strings.TrimPrefix(err.Error(), "bad request, Bad Request: ")
				if description == "message is not modified" || strings.HasPrefix(description, "message is not modified: ") {
					return notification.MessageID, nil
				}
				if description == "message to edit not found" {
					return 0, &monitor.MessageMissingError{Err: err}
				}
			}
			if err == nil {
				return notification.MessageID, nil
			}
		} else {
			params := &bot.SendRichMessageParams{
				ChatID: c.chatID, MessageThreadID: c.threadID, RichMessage: rich,
			}
			if notification.MessageID != 0 {
				params.ReplyParameters = &models.ReplyParameters{
					MessageID: notification.MessageID, AllowSendingWithoutReply: true,
				}
			}
			message, err = c.bot.SendRichMessage(ctx, params)
		}
	}
	var limit *bot.TooManyRequestsError
	if errors.As(err, &limit) {
		return 0, &monitor.RateLimitError{After: time.Duration(limit.RetryAfter) * time.Second}
	}
	if err != nil {
		return 0, err
	}
	if message == nil || message.ID == 0 {
		return 0, fmt.Errorf("Telegram returned no message id")
	}
	return message.ID, nil
}
