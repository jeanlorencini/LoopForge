package telegram

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestSplitCountsUTF16Units(t *testing.T) {
	for name, content := range map[string]string{
		"emoji":      strings.Repeat("😀", 5000),
		"no newline": strings.Repeat("a", 9000),
		"exact max":  strings.Repeat("a", 4096),
		"5000 chars": strings.Repeat("a", 5000),
	} {
		parts := Split(content)
		if strings.Join(parts, "") != content {
			t.Errorf("%s: parts do not rebuild the content", name)
		}
		for _, part := range parts {
			if n := len(utf16.Encode([]rune(part))); n > MaxText {
				t.Errorf("%s: part has %d units", name, n)
			}
		}
	}
	if n := len(Split(strings.Repeat("a", 5000))); n != 2 {
		t.Errorf("5000 chars split into %d parts", n)
	}
}

func TestCallReturnsRefusalAndHidesToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error_code":403,"description":"Forbidden: bot was blocked by the user"}`))
	}))
	c := &Client{HTTP: server.Client(), BaseURL: server.URL}
	err := c.Call(context.Background(), "123:token", "sendMessage", map[string]any{}, nil)
	var refused *Error
	if !errors.As(err, &refused) || refused.Code != 403 || !strings.Contains(refused.Description, "blocked") {
		t.Fatalf("refusal %v", err)
	}
	server.Close()
	err = c.Call(context.Background(), "123:token", "sendMessage", map[string]any{}, nil)
	if err == nil || strings.Contains(err.Error(), "123:token") {
		t.Fatalf("network error %v", err)
	}
}
