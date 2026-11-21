package notification

import (
	"agentflow/internal/domain"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

type TelegramProvider struct {
	HTTP    *http.Client
	BaseURL string
}

func NewTelegramProvider() *TelegramProvider {
	return &TelegramProvider{HTTP: &http.Client{Timeout: 15 * time.Second}, BaseURL: "https://api.telegram.org"}
}

// telegramMaxText is the sendMessage limit: "1-4096 characters after entities parsing".
const telegramMaxText = 4096

func (p *TelegramProvider) call(ctx context.Context, token, method string, params, result any) error {
	body, _ := json.Marshal(params)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/bot"+token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return errors.New("Telegram 请求创建失败")
	}
	req.Header.Set("Content-Type", "application/json")
	rsp, err := p.HTTP.Do(req)
	if err != nil {
		// The error text holds the request URL, which contains the token.
		return errors.New("Telegram 网络请求失败")
	}
	defer rsp.Body.Close()
	var out struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		ErrorCode   int             `json:"error_code"`
		Parameters  struct {
			MigrateToChatID int64 `json:"migrate_to_chat_id"`
		} `json:"parameters"`
	}
	if json.NewDecoder(io.LimitReader(rsp.Body, 2<<20)).Decode(&out) != nil {
		return errors.New("Telegram 响应格式异常")
	}
	if !out.OK {
		switch {
		case out.ErrorCode == http.StatusUnauthorized:
			return &ProviderError{Code: 853004, Source: "Telegram", Message: "Bot Token 无效，请重新填写"}
		case out.ErrorCode == http.StatusForbidden || out.Parameters.MigrateToChatID != 0 || strings.Contains(out.Description, "chat not found"):
			return &ProviderError{Code: 404, Source: "Telegram", Message: "Bot 已不在该会话中或群已升级，请重新选择接收会话"}
		}
		return &ProviderError{Code: out.ErrorCode, Source: "Telegram", Message: out.Description}
	}
	if result != nil && json.Unmarshal(out.Result, result) != nil {
		return errors.New("Telegram 响应格式异常")
	}
	return nil
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
	for _, part := range splitTelegramText(content) {
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

func splitTelegramText(text string) []string {
	var parts []string
	for text != "" {
		units, end, lastBreak := 0, len(text), -1
		for i, r := range text {
			units += utf16.RuneLen(r)
			if units > telegramMaxText {
				end = i
				break
			}
			if r == '\n' {
				lastBreak = i + 1
			}
		}
		if end < len(text) && lastBreak > 0 {
			end = lastBreak
		}
		parts = append(parts, text[:end])
		text = text[end:]
	}
	return parts
}
