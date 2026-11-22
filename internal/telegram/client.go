// Package telegram calls the Telegram Bot API. Both the notification provider
// and the conversation transport use it, so the token never leaks from either.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"unicode/utf16"
)

// MaxText is the sendMessage limit: "1-4096 characters after entities parsing".
const MaxText = 4096

const BaseURL = "https://api.telegram.org"

type Client struct {
	HTTP    *http.Client
	BaseURL string
}

// Error is a request the Bot API answered with ok:false.
type Error struct {
	Code            int
	Description     string
	MigrateToChatID int64
}

func (e *Error) Error() string { return "Telegram: " + e.Description }

func (c *Client) Call(ctx context.Context, token, method string, params, result any) error {
	body, _ := json.Marshal(params)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/bot"+token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return errors.New("Telegram: falha ao montar o pedido")
	}
	req.Header.Set("Content-Type", "application/json")
	rsp, err := c.HTTP.Do(req)
	if err != nil {
		// The error text holds the request URL, which contains the token.
		return errors.New("Telegram: falha de rede")
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
		return errors.New("Telegram: resposta em formato inesperado")
	}
	if !out.OK {
		return &Error{Code: out.ErrorCode, Description: out.Description, MigrateToChatID: out.Parameters.MigrateToChatID}
	}
	if result != nil && json.Unmarshal(out.Result, result) != nil {
		return errors.New("Telegram: resposta em formato inesperado")
	}
	return nil
}

// Split cuts text into sendMessage-sized parts, preferring line breaks. The
// limit counts UTF-16 units, so an emoji takes two.
func Split(text string) []string {
	var parts []string
	for text != "" {
		units, end, lastBreak := 0, len(text), -1
		for i, r := range text {
			units += utf16.RuneLen(r)
			if units > MaxText {
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
