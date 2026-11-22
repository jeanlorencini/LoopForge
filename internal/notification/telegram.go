package notification

import (
	"agentflow/internal/domain"
	"agentflow/internal/telegram"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

type TelegramProvider struct {
	HTTP    *http.Client
	BaseURL string
}

func NewTelegramProvider() *TelegramProvider {
	return &TelegramProvider{HTTP: &http.Client{Timeout: 15 * time.Second}, BaseURL: telegram.BaseURL}
}

func (p *TelegramProvider) call(ctx context.Context, token, method string, params, result any) error {
	err := (&telegram.Client{HTTP: p.HTTP, BaseURL: p.BaseURL}).Call(ctx, token, method, params, result)
	var refused *telegram.Error
	if !errors.As(err, &refused) {
		return err
	}
	switch {
	case refused.Code == http.StatusUnauthorized:
		return &ProviderError{Code: 853004, Source: "Telegram", Message: "Token do bot inválido. Preencha de novo."}
	case refused.Code == http.StatusForbidden || refused.MigrateToChatID != 0 || strings.Contains(refused.Description, "chat not found"):
		return &ProviderError{Code: 404, Source: "Telegram", Message: "O bot saiu deste chat ou o grupo foi migrado. Escolha o chat de destino de novo."}
	}
	return &ProviderError{Code: refused.Code, Source: "Telegram", Message: refused.Description}
}

func (p *TelegramProvider) BotID(ctx context.Context, token string) (string, error) {
	var me struct {
		ID int64 `json:"id"`
	}
	if err := p.call(ctx, token, "getMe", map[string]any{}, &me); err != nil {
		return "", err
	}
	return domain.TelegramBotIDPrefix + strconv.FormatInt(me.ID, 10), nil
}

// Sessions lists the chats seen in the last 100 updates. The Bot API has no
// chat list, and it keeps updates for 24 hours only.
func (p *TelegramProvider) Sessions(ctx context.Context, _, token string) ([]domain.BotChat, error) {
	type chat struct {
		ID        int64  `json:"id"`
		Type      string `json:"type"`
		Title     string `json:"title"`
		Username  string `json:"username"`
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
	}
	var updates []map[string]json.RawMessage
	if err := p.call(ctx, token, "getUpdates", map[string]any{"offset": -100, "limit": 100}, &updates); err != nil {
		return nil, err
	}
	seen := map[int64]int64{}
	var chats []domain.BotChat
	for _, update := range updates {
		// message, channel_post, my_chat_member and similar fields carry chat and date.
		for _, raw := range update {
			var item struct {
				Chat *chat `json:"chat"`
				Date int64 `json:"date"`
			}
			if json.Unmarshal(raw, &item) != nil || item.Chat == nil || item.Chat.ID == 0 {
				continue
			}
			c := item.Chat
			name := strings.TrimSpace(c.FirstName + " " + c.LastName)
			if c.Title != "" {
				name = c.Title
			} else if name == "" && c.Username != "" {
				name = "@" + c.Username
			}
			kind := "group"
			if c.Type == "private" {
				kind = "single"
			}
			entry := domain.BotChat{ID: strconv.FormatInt(c.ID, 10), Name: name, Type: kind, LastMessageTime: time.Unix(item.Date, 0).UTC().Format("2006-01-02 15:04 UTC")}
			if last, ok := seen[c.ID]; ok {
				if item.Date >= last {
					seen[c.ID] = item.Date
					i := slices.IndexFunc(chats, func(b domain.BotChat) bool { return b.ID == entry.ID })
					chats[i] = entry
				}
				continue
			}
			seen[c.ID] = item.Date
			chats = append(chats, entry)
		}
	}
	slices.SortStableFunc(chats, func(a, b domain.BotChat) int { return strings.Compare(b.LastMessageTime, a.LastMessageTime) })
	return chats, nil
}

// errPartialDelivery marks a send that stopped after some parts arrived:
// retrying it would repeat those parts in the chat.
var errPartialDelivery = errors.New("Telegram 消息仅部分送达")

func (p *TelegramProvider) Send(ctx context.Context, _, token, target, content string) error {
	if strings.TrimSpace(content) == "" {
		return errors.New("通知正文不能为空")
	}
	sent := 0
	for _, part := range telegram.Split(content) {
		if strings.TrimSpace(part) == "" {
			continue // sendMessage rejects blank text
		}
		if err := p.call(ctx, token, "sendMessage", map[string]any{"chat_id": target, "text": part}, nil); err != nil {
			if sent > 0 {
				return fmt.Errorf("%w（已发送 %d 段）：%w", errPartialDelivery, sent, err)
			}
			return err
		}
		sent++
	}
	return nil
}
