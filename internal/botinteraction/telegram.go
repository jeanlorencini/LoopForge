package botinteraction

import (
	"agentflow/internal/domain"
	"agentflow/internal/repo"
	"agentflow/internal/telegram"
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf16"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

// telegramPollTimeout is the getUpdates long-poll wait; the HTTP client deadline
// must stay above it.
const telegramPollTimeout = 25

type tgUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}
type tgMessage struct {
	MessageID int64   `json:"message_id"`
	From      *tgUser `json:"from"`
	Chat      struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	Date    int64      `json:"date"`
	Text    string     `json:"text"`
	ReplyTo *tgMessage `json:"reply_to_message"`
}
type tgUpdate struct {
	UpdateID      int64      `json:"update_id"`
	Message       *tgMessage `json:"message"`
	CallbackQuery *struct {
		ID      string     `json:"id"`
		From    tgUser     `json:"from"`
		Message *tgMessage `json:"message"`
		Data    string     `json:"data"`
	} `json:"callback_query"`
}

// telegramConn polls one bot token and answers through the Bot API. It runs
// only under the connection lease, so each token has a single getUpdates caller.
type telegramConn struct {
	s         *Service
	bot       domain.NotificationBot
	token     string
	api       *telegram.Client
	done      chan struct{}
	cancel    context.CancelFunc
	closeOnce sync.Once
	offset    int64
	// lastCreated keeps queue order for messages sent in the same second.
	lastCreated time.Time
	// resolved holds notification messages already edited with a final result,
	// so a second queued tap answers in chat instead of overwriting it.
	resolved sync.Map
}

// pollTelegram checks the token with one getUpdates before reporting the bot
// connected, so a bad token or an active webhook shows as a connection error.
func (s *Service) pollTelegram(ctx context.Context, bot domain.NotificationBot, token string) (transport, error) {
	ctx, cancel := context.WithCancel(ctx)
	c := &telegramConn{s: s, bot: bot, token: token, api: &telegram.Client{HTTP: &http.Client{Timeout: (telegramPollTimeout + 15) * time.Second}, BaseURL: s.TelegramURL}, done: make(chan struct{}), cancel: cancel}
	updates, err := c.getUpdates(ctx, 0)
	if err != nil {
		cancel()
		return nil, err
	}
	go func() {
		defer c.close()
		for {
			if err := c.handle(ctx, updates); err != nil {
				log.Printf("telegram update failed bot=%s: %v", bot.ID, err)
				return
			}
			if updates, err = c.getUpdates(ctx, telegramPollTimeout); err != nil {
				if ctx.Err() == nil {
					log.Printf("telegram getUpdates failed bot=%s: %v", bot.ID, err)
				}
				return
			}
		}
	}()
	return c, nil
}

func (c *telegramConn) getUpdates(ctx context.Context, timeout int) ([]tgUpdate, error) {
	var updates []tgUpdate
	// allowed_updates is remembered by Telegram, so every call states it.
	err := c.api.Call(ctx, c.token, "getUpdates", map[string]any{"offset": c.offset, "timeout": timeout, "allowed_updates": []string{"message", "callback_query"}}, &updates)
	return updates, err
}

// handle queues a batch in order. The offset moves past an update only after
// it is queued or deliberately ignored, so a failed write is fetched again.
func (c *telegramConn) handle(ctx context.Context, updates []tgUpdate) error {
	for _, u := range updates {
		var err error
		if u.CallbackQuery != nil {
			err = c.tap(ctx, u)
		} else if u.Message != nil {
			err = c.message(ctx, u.Message)
		}
		if err != nil {
			if !dataRefused(err) {
				return err
			}
			// A data error repeats on every refetch, so skip the update or it blocks every later one.
			log.Printf("telegram update skipped bot=%s update=%d: %v", c.bot.ID, u.UpdateID, err)
		}
		c.offset = u.UpdateID + 1
	}
	return nil
}

// dataRefused reports a MySQL error about the data itself (a character a
// utf8mb3 column cannot hold, a value too long), which a retry cannot fix.
func dataRefused(err error) bool {
	var sqlErr *mysql.MySQLError
	return errors.As(err, &sqlErr) && (sqlErr.Number == 1366 || sqlErr.Number == 3988 || sqlErr.Number == 1406)
}

func (c *telegramConn) message(ctx context.Context, m *tgMessage) error {
	if m.Chat.Type != "private" || m.From == nil || m.From.ID != m.Chat.ID || strings.TrimSpace(m.Text) == "" {
		return nil
	}
	chat := strconv.FormatInt(m.Chat.ID, 10)
	item := domain.BotInteraction{ID: repo.BotMessageID(c.bot.BotID, chat+":"+strconv.FormatInt(m.MessageID, 10)), BotID: c.bot.ID, SenderID: chat, Content: domain.NotificationText(stripBotMention(m.Text), 16000), CreatedAt: c.createdAt(m.Date)}
	if r := m.ReplyTo; r != nil && r.From != nil && r.From.IsBot && strconv.FormatInt(r.From.ID, 10) == strings.TrimPrefix(c.bot.BotID, domain.TelegramBotIDPrefix) {
		item.Quote = domain.NotificationText(r.Text, 8000)
	}
	if err := c.s.accept(ctx, item, "single", chat, c); err != nil {
		return err
	}
	name := strings.TrimSpace(m.From.FirstName + " " + m.From.LastName)
	if name == "" {
		name = m.From.Username
	}
	// The name only labels the target list, so a failed write must not hold up the message.
	if err := c.s.Store.NameBotConversation(repo.BotConversationID(c.bot.ID, "single", chat), cardText(name, 191)); err != nil {
		log.Printf("telegram chat name not saved bot=%s chat=%s: %v", c.bot.ID, chat, err)
	}
	return nil
}

// createdAt dates a message by when it was sent, so process refuses a backlog
// Telegram kept while the platform was down. A missing date counts as old.
func (c *telegramConn) createdAt(date int64) time.Time {
	now := time.Now().UTC()
	if date <= 0 {
		return now.Add(-24 * time.Hour)
	}
	at := time.Unix(date, 0).UTC()
	if at.After(now) {
		at = now
	}
	// Bump only within the same second; an older message must stay old.
	if !at.After(c.lastCreated) && c.lastCreated.Sub(at) < time.Second {
		at = c.lastCreated.Add(time.Millisecond)
	}
	if at.After(c.lastCreated) {
		c.lastCreated = at
	}
	return at
}

// stripBotMention turns "/bind@SomeBot CODE" into "/bind CODE".
func stripBotMention(text string) string {
	if !strings.HasPrefix(text, "/") {
		return text
	}
	end := strings.IndexFunc(text, unicode.IsSpace)
	if end < 0 {
		end = len(text)
	}
	if at := strings.IndexByte(text[:end], '@'); at > 0 {
		return text[:at] + text[end:]
	}
	return text
}

func (c *telegramConn) tap(ctx context.Context, u tgUpdate) error {
	q := u.CallbackQuery
	// Telegram shows a spinner on the button until the tap is answered.
	_ = c.api.Call(ctx, c.token, "answerCallbackQuery", map[string]any{"callback_query_id": q.ID}, nil)
	m := q.Message
	if m == nil || m.Chat.Type != "private" || q.From.ID != m.Chat.ID {
		return nil
	}
	action, taskID, _ := strings.Cut(q.Data, ":")
	if _, ok := telegramActionLabels[action]; !ok || taskID == "" {
		return nil
	}
	card, err := c.s.Store.BotCardByTaskID(c.bot.ID, taskID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	chat := strconv.FormatInt(m.Chat.ID, 10)
	// Quote keeps the notification text, which the result edit rewrites.
	item := domain.BotInteraction{ID: repo.BotMessageID(c.bot.BotID, "callback:"+q.ID), BotID: c.bot.ID, SenderID: chat, RequestID: strconv.FormatInt(m.MessageID, 10), Quote: m.Text, Action: action, DeliveryID: card.DeliveryID, CardTaskID: taskID}
	return c.s.accept(ctx, item, "single", chat, c)
}

func (c *telegramConn) closed() <-chan struct{} { return c.done }

func (c *telegramConn) close() {
	c.closeOnce.Do(func() {
		c.cancel()
		close(c.done)
	})
}

// acknowledge sends nothing: Telegram shows "typing…" during the turn instead.
func (c *telegramConn) acknowledge(context.Context, domain.BotInteraction) {}

func (c *telegramConn) progressTurn(ctx context.Context, item domain.BotInteraction) (context.Context, func()) {
	typingCtx, stop := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			// The typing status lasts at most 5 seconds.
			_ = c.api.Call(typingCtx, c.token, "sendChatAction", map[string]any{"chat_id": item.SenderID, "action": "typing"}, nil)
			select {
			case <-typingCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return ctx, func() { stop(); <-stopped }
}

// call turns a Bot API refusal into a ReplyError, which fails only this item;
// a network error still ends the connection.
func (c *telegramConn) call(ctx context.Context, method string, params, result any) error {
	err := c.api.Call(ctx, c.token, method, params, result)
	var refused *telegram.Error
	if errors.As(err, &refused) {
		if strings.Contains(refused.Description, "message is not modified") {
			return nil
		}
		return &ReplyError{Code: refused.Code, Message: refused.Error()}
	}
	return err
}

func (c *telegramConn) reply(ctx context.Context, _ domain.BotInteraction, chatID, text string) error {
	for _, part := range telegram.Split(text) {
		if err := c.call(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": part}, nil); err != nil {
			return err
		}
	}
	return nil
}

var telegramActionLabels = map[string]string{"approve": "Aprovar", "retry": "Repetir", "reject": "Mandar para retrabalho", "pass": "Liberar manualmente", "resume": "Continuar", "decline": "Não repetir"}

// sendCard sends the notification text with one inline button per action.
// The buttons go on the last part, the one a tap edits.
func (c *telegramConn) sendCard(ctx context.Context, item domain.BotInteraction, chatID, taskID string, actions []map[string]any) error {
	keyboard := [][]map[string]string{}
	for _, action := range actions {
		key, _ := action["key"].(string)
		keyboard = append(keyboard, []map[string]string{{"text": telegramActionLabels[key], "callback_data": key + ":" + taskID}})
	}
	parts := telegram.Split(item.Content)
	for i, part := range parts {
		params := map[string]any{"chat_id": chatID, "text": part}
		if i == len(parts)-1 {
			params["reply_markup"] = map[string]any{"inline_keyboard": keyboard}
		}
		if err := c.call(ctx, "sendMessage", params, nil); err != nil {
			return err
		}
	}
	return nil
}

func (c *telegramConn) actionStarted(ctx context.Context, item domain.BotInteraction) {
	if _, done := c.resolved.Load(cardKey(item)); !done {
		_ = c.editCard(ctx, item, "Processando…")
	}
}

func (c *telegramConn) updateActionCard(ctx context.Context, item domain.BotInteraction, result string, finish bool) error {
	if _, done := c.resolved.Load(cardKey(item)); done {
		return nil
	}
	err := c.editCard(ctx, item, result)
	if err == nil && finish {
		c.resolved.Store(cardKey(item), struct{}{})
	}
	return err
}

// cardKey names a notification message; message ids repeat across chats.
func cardKey(item domain.BotInteraction) string { return item.SenderID + ":" + item.RequestID }

func (c *telegramConn) editCard(ctx context.Context, item domain.BotInteraction, line string) error {
	return c.call(ctx, "editMessageText", map[string]any{"chat_id": item.SenderID, "message_id": item.RequestID, "text": fitTelegram(item.Quote, "\n\n"+line), "reply_markup": map[string]any{"inline_keyboard": [][]map[string]string{}}}, nil)
}

func fitTelegram(head, tail string) string {
	budget := telegram.MaxText - len(utf16.Encode([]rune(tail)))
	units := 0
	for i, r := range head {
		units += utf16.RuneLen(r)
		if units > budget {
			return head[:i] + tail
		}
	}
	return head + tail
}
