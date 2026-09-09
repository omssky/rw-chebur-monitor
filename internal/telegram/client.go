package telegram

import (
	"context"
	"errors"
	"net/http"
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

func (c *Client) Send(ctx context.Context, text string) error {
	disabled := true
	_, err := c.bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: c.chatID, MessageThreadID: c.threadID, Text: text,
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: &disabled},
	})
	var limit *bot.TooManyRequestsError
	if errors.As(err, &limit) {
		return &monitor.RateLimitError{After: time.Duration(limit.RetryAfter) * time.Second}
	}
	return err
}
