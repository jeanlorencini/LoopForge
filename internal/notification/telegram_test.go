package notification

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf16"
)

func telegramServer(t *testing.T, handle func(method string, body map[string]any) string) *TelegramProvider {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, ok := strings.CutPrefix(r.URL.Path, "/bot123:token/")
		if !ok || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(handle(method, body)))
	}))
	t.Cleanup(server.Close)
	return &TelegramProvider{HTTP: server.Client(), BaseURL: server.URL}
}

func TestTelegramSessionsListRecentChats(t *testing.T) {
	var offset any
	p := telegramServer(t, func(method string, body map[string]any) string {
		if method != "getUpdates" {
			t.Errorf("method %s", method)
		}
		offset = body["offset"]
		return `{"ok":true,"result":[
			{"update_id":1,"message":{"date":100,"chat":{"id":42,"type":"private","first_name":"Ana","last_name":"Lima"}}},
			{"update_id":2,"my_chat_member":{"date":200,"chat":{"id":-100777,"type":"supergroup","title":"Dev"}}},
			{"update_id":3,"callback_query":{"id":"x"}},
			{"update_id":4,"message":{"date":300,"chat":{"id":42,"type":"private","first_name":"Ana","last_name":"Lima"}}}
		]}`
	})
	chats, err := p.Sessions(context.Background(), "telegram-123", "123:token")
	if err != nil {
		t.Fatal(err)
	}
	if offset != float64(-100) {
		t.Errorf("offset %v, want the last 100 updates", offset)
	}
	if len(chats) != 2 || chats[0].ID != "42" || chats[0].Name != "Ana Lima" || chats[0].Type != "single" || chats[1].ID != "-100777" || chats[1].Name != "Dev" || chats[1].Type != "group" {
		t.Fatalf("chats %+v", chats)
	}
}

func TestTelegramSendSplitsLongPlainText(t *testing.T) {
	var parts []string
	p := telegramServer(t, func(method string, body map[string]any) string {
		if method != "sendMessage" || body["chat_id"] != "-100777" || body["parse_mode"] != nil {
			t.Errorf("request %s %v", method, body)
		}
		parts = append(parts, body["text"].(string))
		return `{"ok":true,"result":{"message_id":1}}`
	})
	content := strings.Repeat("a", 3000) + "\n" + strings.Repeat("é", 3000)
	if err := p.Send(context.Background(), "telegram-123", "123:token", "-100777", content); err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 || strings.Join(parts, "") != content {
		t.Fatalf("parts %d do not rebuild the content", len(parts))
	}
	for _, part := range parts {
		if n := len(utf16.Encode([]rune(part))); n > 4096 {
			t.Fatalf("part has %d characters", n)
		}
	}
}

func TestTelegramSendSkipsBlankPartsAndStopsAfterPartialDelivery(t *testing.T) {
	var texts []string
	p := telegramServer(t, func(_ string, body map[string]any) string {
		texts = append(texts, body["text"].(string))
		if len(texts) == 2 {
			return `{"ok":false,"error_code":500,"description":"Internal Server Error"}`
		}
		return `{"ok":true,"result":{"message_id":1}}`
	})
	if err := p.Send(context.Background(), "telegram-123", "123:token", "1", strings.Repeat("a", 4096)+"\n"); err != nil || len(texts) != 1 {
		t.Fatalf("blank tail: err %v, sent %d", err, len(texts))
	}
	texts = nil
	err := p.Send(context.Background(), "telegram-123", "123:token", "1", strings.Repeat("a", 9000))
	if !errors.Is(err, errPartialDelivery) || len(texts) != 2 {
		t.Fatalf("partial: err %v, sent %d", err, len(texts))
	}
}

func TestTelegramErrorsMapToDeliveryStatus(t *testing.T) {
	for name, tc := range map[string]struct {
		reply string
		code  int
	}{
		"bad token":     {`{"ok":false,"error_code":401,"description":"Unauthorized"}`, 853004},
		"removed":       {`{"ok":false,"error_code":403,"description":"Forbidden: bot was kicked"}`, 404},
		"group upgrade": {`{"ok":false,"error_code":400,"description":"Bad Request: group chat was upgraded","parameters":{"migrate_to_chat_id":-100888}}`, 404},
		"chat gone":     {`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`, 404},
	} {
		p := telegramServer(t, func(string, map[string]any) string { return tc.reply })
		err := p.Send(context.Background(), "telegram-123", "123:token", "1", "oi")
		var pe *ProviderError
		if !errors.As(err, &pe) || pe.Code != tc.code {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if msg := err.Error(); strings.Contains(msg, "企业微信") || strings.Contains(msg, "123:token") {
			t.Errorf("%s: message %q", name, msg)
		}
	}
}

func TestTelegramNetworkErrorHidesToken(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	p := &TelegramProvider{HTTP: server.Client(), BaseURL: server.URL}
	err := p.Send(context.Background(), "telegram-123", "123:token", "1", "oi")
	if err == nil || strings.Contains(err.Error(), "123:token") {
		t.Fatalf("error %v", err)
	}
}
