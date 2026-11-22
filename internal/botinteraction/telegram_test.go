package botinteraction

import (
	"agentflow/internal/domain"
	"agentflow/internal/repo"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/go-sql-driver/mysql"
)

type tgCall struct {
	Method string
	Body   map[string]any
}

// fakeTelegram serves getUpdates from a queue and records every other call.
type fakeTelegram struct {
	mu      sync.Mutex
	updates []map[string]any
	nextID  int64
	calls   []tgCall
	// refuse returns a raw ok:false body for a call, when set.
	refuse func(method string, body map[string]any) string
}

func newFakeTelegram(t *testing.T) (*fakeTelegram, *httptest.Server) {
	f := &fakeTelegram{nextID: 1}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, ok := strings.CutPrefix(r.URL.Path, "/bot123:token/")
		if !ok {
			t.Errorf("path %s", r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.calls = append(f.calls, tgCall{method, body})
		refuse := f.refuse
		f.mu.Unlock()
		if refuse != nil {
			if out := refuse(method, body); out != "" {
				_, _ = w.Write([]byte(out))
				return
			}
		}
		if method != "getUpdates" {
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
			return
		}
		offset, _ := body["offset"].(float64)
		f.mu.Lock()
		out := []map[string]any{}
		for _, u := range f.updates {
			if u["update_id"].(int64) >= int64(offset) {
				out = append(out, u)
			}
		}
		f.mu.Unlock()
		if len(out) == 0 {
			time.Sleep(20 * time.Millisecond)
		}
		data, _ := json.Marshal(map[string]any{"ok": true, "result": out})
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeTelegram) push(update map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	update["update_id"] = f.nextID
	f.nextID++
	f.updates = append(f.updates, update)
}

func (f *fakeTelegram) sent(method string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []map[string]any{}
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c.Body)
		}
	}
	return out
}

func (f *fakeTelegram) sentTo(chat string) []string {
	out := []string{}
	for _, body := range f.sent("sendMessage") {
		if body["chat_id"] == chat {
			out = append(out, body["text"].(string))
		}
	}
	return out
}

func tgPrivate(chat, msgID int64, date int64, text string) map[string]any {
	return map[string]any{"message": map[string]any{"message_id": msgID, "date": date, "text": text, "from": map[string]any{"id": chat, "first_name": "Ana"}, "chat": map[string]any{"id": chat, "type": "private"}}}
}

func startTelegramService(t *testing.T) (*Service, *repo.Store, *fakeTelegram, domain.NotificationBot) {
	store, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	bot := domain.NotificationBot{ID: "tg-bot", BotID: "telegram-123", Provider: domain.BotProviderTelegram, ProjectID: "p", OwnerID: 42, Enabled: true, InteractionEnabled: true}
	if err = store.SaveNotificationBot(bot, "123:token"); err != nil {
		t.Fatal(err)
	}
	fake, srv := newFakeTelegram(t)
	service := New(store)
	service.TelegramURL = srv.URL
	return service, store, fake, bot
}

func startPolling(t *testing.T, service *Service) {
	ctx, cancel := context.WithCancel(context.Background())
	service.Start(ctx)
	t.Cleanup(func() { cancel(); <-service.Done })
}

func TestTelegramConversation(t *testing.T) {
	service, store, fake, _ := startTelegramService(t)
	var mu sync.Mutex
	var seen []domain.BotInteraction
	service.Generate = func(ctx context.Context, b domain.NotificationBot, c domain.BotConversation, h []domain.BotInteraction, item domain.BotInteraction) (string, error) {
		mu.Lock()
		seen = append(seen, item)
		mu.Unlock()
		if item.Content == "longo" {
			return strings.Repeat("a", 5000), nil
		}
		return "R: " + item.Content, nil
	}
	generated := func(content string) []domain.BotInteraction {
		mu.Lock()
		defer mu.Unlock()
		out := []domain.BotInteraction{}
		for _, item := range seen {
			if item.Content == content {
				out = append(out, item)
			}
		}
		return out
	}
	now := time.Now().Unix()
	fake.push(tgPrivate(7, 1, now, "qual o status?"))
	group := tgPrivate(8, 1, now, "@bot status")
	group["message"].(map[string]any)["chat"] = map[string]any{"id": -100, "type": "group"}
	fake.push(group)
	forwarded := tgPrivate(9, 1, now, "de outro")
	forwarded["message"].(map[string]any)["from"] = map[string]any{"id": 10}
	fake.push(forwarded)
	fake.push(map[string]any{"message": map[string]any{"message_id": 2, "date": now, "photo": []any{}, "from": map[string]any{"id": 7}, "chat": map[string]any{"id": 7, "type": "private"}}})
	fake.push(tgPrivate(11, 1, now-3600, "antiga"))
	fake.push(tgPrivate(12, 1, 0, "sem data"))
	fake.push(tgPrivate(13, 1, now, "/new@LoopForgeBot"))
	fake.push(tgPrivate(13, 2, now, "/bind@LoopForgeBot 123456"))
	quoted := tgPrivate(14, 2, now, "e esse?")
	quoted["message"].(map[string]any)["reply_to_message"] = map[string]any{"message_id": 1, "text": "Requisito X em espera", "from": map[string]any{"id": 123, "is_bot": true}}
	fake.push(quoted)
	own := tgPrivate(15, 2, now, "minha citação")
	own["message"].(map[string]any)["reply_to_message"] = map[string]any{"message_id": 1, "text": "eu disse", "from": map[string]any{"id": 15}}
	fake.push(own)
	fake.push(tgPrivate(16, 1, now, "primeira"))
	fake.push(tgPrivate(16, 2, now, "segunda"))
	fake.push(tgPrivate(17, 1, now, "longo"))
	fake.push(tgPrivate(7, 1, now, "qual o status?")) // redelivered update
	startPolling(t, service)

	eventually(t, func() bool { return len(fake.sentTo("17")) == 2 })
	if got := fake.sentTo("7"); len(got) != 1 || got[0] != "R: qual o status?" {
		t.Fatalf("chat 7 replies %q", got)
	}
	if len(generated("qual o status?")) != 1 {
		t.Fatal("redelivered update answered twice")
	}
	for _, chat := range []string{"8", "-100", "9", "10"} {
		if len(fake.sentTo(chat)) != 0 {
			t.Fatalf("chat %s answered", chat)
		}
	}
	if len(generated("@bot status")) != 0 || len(generated("de outro")) != 0 || len(generated("antiga")) != 0 || len(generated("sem data")) != 0 {
		t.Fatal("ignored or stale message reached Generate")
	}
	for _, chat := range []string{"11", "12"} {
		if got := fake.sentTo(chat); len(got) != 1 || strings.HasPrefix(got[0], "R:") {
			t.Fatalf("stale chat %s replies %q", chat, got)
		}
	}
	if len(generated("/new")) != 0 || len(generated("/bind 123456")) != 1 {
		t.Fatal("bot mention not stripped from commands")
	}
	if q := generated("e esse?"); len(q) != 1 || q[0].Quote != "Requisito X em espera" {
		t.Fatalf("bot reply quote %+v", q)
	}
	if q := generated("minha citação"); len(q) != 1 || q[0].Quote != "" {
		t.Fatalf("own reply quote %+v", q)
	}
	mu.Lock()
	first, second := -1, -1
	for i, item := range seen {
		if item.Content == "primeira" {
			first = i
		} else if item.Content == "segunda" {
			second = i
		}
	}
	mu.Unlock()
	if first < 0 || second < first {
		t.Fatalf("same-second order first=%d second=%d", first, second)
	}
	for _, body := range fake.sent("sendMessage") {
		if _, ok := body["parse_mode"]; ok {
			t.Fatal("reply uses parse_mode")
		}
	}
	for _, body := range fake.sent("getUpdates") {
		allowed, _ := body["allowed_updates"].([]any)
		if len(allowed) != 2 || allowed[0] != "message" || allowed[1] != "callback_query" {
			t.Fatalf("allowed_updates %v", body["allowed_updates"])
		}
	}
	conv, err := store.BotConversation(repo.BotConversationID("tg-bot", "single", "7"))
	if err != nil || conv.ChatName != "Ana" {
		t.Fatalf("chat name %+v %v", conv, err)
	}
}

func TestTelegramRefusedReplyFailsOnlyThatItem(t *testing.T) {
	service, store, fake, _ := startTelegramService(t)
	service.Generate = func(ctx context.Context, b domain.NotificationBot, c domain.BotConversation, h []domain.BotInteraction, item domain.BotInteraction) (string, error) {
		return "R: " + item.Content, nil
	}
	fake.refuse = func(method string, body map[string]any) string {
		if method == "sendMessage" && body["chat_id"] == "99" {
			return `{"ok":false,"error_code":403,"description":"Forbidden: bot was blocked by the user"}`
		}
		return ""
	}
	now := time.Now().Unix()
	fake.push(tgPrivate(99, 1, now, "oi"))
	fake.push(tgPrivate(7, 1, now, "oi"))
	startPolling(t, service)
	eventually(t, func() bool { return len(fake.sentTo("7")) == 1 })
	item, err := store.BotInteraction(repo.BotMessageID("telegram-123", "99:1"))
	if err != nil || item.Status != "reply_failed" || !strings.Contains(item.LastError, "blocked") {
		t.Fatalf("blocked chat item %+v %v", item, err)
	}
}

func TestTelegramFailedNameWriteDoesNotBlockLaterMessages(t *testing.T) {
	service, store, fake, _ := startTelegramService(t)
	service.Generate = func(ctx context.Context, b domain.NotificationBot, c domain.BotConversation, h []domain.BotInteraction, item domain.BotInteraction) (string, error) {
		return "R: " + item.Content, nil
	}
	if err := store.DB.Exec("CREATE TRIGGER refuse_chat_name BEFORE UPDATE OF chat_name ON bot_conversations BEGIN SELECT RAISE(ABORT, 'refused'); END").Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	fake.push(tgPrivate(31, 1, now, "oi"))
	fake.push(tgPrivate(32, 1, now, "oi"))
	startPolling(t, service)
	eventually(t, func() bool { return len(fake.sentTo("31")) == 1 && len(fake.sentTo("32")) == 1 })
}

func TestDataRefused(t *testing.T) {
	if !dataRefused(fmt.Errorf("enqueue: %w", &mysql.MySQLError{Number: 1366})) || dataRefused(&mysql.MySQLError{Number: 1213}) || dataRefused(errors.New("connection reset")) {
		t.Fatal("dataRefused misclassifies")
	}
}

func TestTelegramWebhookConflictIsAConnectionError(t *testing.T) {
	service, store, fake, bot := startTelegramService(t)
	fake.refuse = func(method string, body map[string]any) string {
		if method == "getUpdates" {
			return `{"ok":false,"error_code":409,"description":"Conflict: can't use getUpdates method while webhook is active; use deleteWebhook to delete the webhook first"}`
		}
		return ""
	}
	startPolling(t, service)
	eventually(t, func() bool {
		state, _ := store.BotConnectionState(bot.BotID)
		return state.State == "error" && strings.Contains(state.LastError, "webhook is active")
	})
	if len(fake.sent("deleteWebhook")) != 0 {
		t.Fatal("platform deleted the webhook")
	}
}

func TestTelegramNotificationButtons(t *testing.T) {
	service, store, fake, bot := startTelegramService(t)
	runState := domain.Run{
		ID: "tg-run", Status: "waiting",
		WorkflowSnapshot: domain.WorkflowDefinition{Name: "WF", Nodes: []domain.WorkflowNode{{ID: "n", Name: "Revisão", RejectTo: "fix"}, {ID: "fix", Name: "Correção"}}},
		Nodes:            map[string]*domain.NodeRun{"n": {NodeID: "n", Status: "waiting_approval", Attempt: 1}},
	}
	if err := store.PutRun(runState); err != nil {
		t.Fatal(err)
	}
	delivery := domain.NotificationDelivery{ID: "tg-delivery", BotID: bot.ID, ProjectID: bot.ProjectID, RunID: runState.ID, NodeID: "n", Attempt: 1, EventType: "waiting", SourceName: "Revisão", TargetType: "single", TargetID: "7", Status: "sent", CreatedAt: time.Now()}
	if err := store.CreateNotificationDelivery(delivery); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var order []string
	actions := 0
	service.Action = func(ctx context.Context, b domain.NotificationBot, item domain.BotInteraction, c domain.BotConversation) (string, error) {
		if item.SenderID == "9" {
			return "Vincule sua conta.", ErrCardActionDeferred
		}
		if _, err := service.NotificationCardTask(b, item, c); err != nil {
			return "Este aviso expirou.", nil
		}
		ActionStarted(ctx)
		mu.Lock()
		order = append(order, "action")
		actions++
		mu.Unlock()
		runState.Nodes["n"].Status = "succeeded"
		if err := store.PutRun(runState); err != nil {
			t.Error(err)
		}
		return "Aprovado.", nil
	}
	fake.refuse = func(method string, body map[string]any) string {
		if method == "answerCallbackQuery" {
			mu.Lock()
			order = append(order, "answer")
			mu.Unlock()
		}
		return ""
	}
	startPolling(t, service)
	queued := make(chan error, 1)
	content := "### Aguardando sua ação\nRequisito: Login\n" + strings.Repeat("contexto da revisão\n", 270)
	go func() {
		queued <- service.QueueNotificationCard(context.Background(), bot, delivery, content, "https://loopforge.example/#/runs/tg-run")
	}()
	select {
	case err := <-queued:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("card not sent")
	}
	parts := fake.sent("sendMessage")
	if len(parts) != 2 || parts[0]["reply_markup"] != nil {
		t.Fatalf("card parts %d", len(parts))
	}
	last := parts[1]["text"].(string)
	if !strings.Contains(last, "Retrabalho volta para: Correção") || !strings.Contains(last, "Detalhes: https://loopforge.example/#/runs/tg-run") {
		t.Fatalf("card tail %q", last)
	}
	keyboard := parts[1]["reply_markup"].(map[string]any)["inline_keyboard"].([]any)
	data := map[string]string{}
	for _, row := range keyboard {
		button := row.([]any)[0].(map[string]any)
		cb := button["callback_data"].(string)
		if len(cb) > 64 || button["text"] == "" {
			t.Fatalf("button %v", button)
		}
		action, _, _ := strings.Cut(cb, ":")
		data[action] = cb
	}
	if data["approve"] == "" || data["reject"] == "" {
		t.Fatalf("buttons %v", data)
	}
	_, taskID, _ := strings.Cut(data["approve"], ":")
	tap := func(id string, chat int64, cbData string) {
		fake.push(map[string]any{"callback_query": map[string]any{"id": id, "data": cbData, "from": map[string]any{"id": chat}, "message": map[string]any{"message_id": 500, "text": last, "chat": map[string]any{"id": chat, "type": "private"}}}})
	}
	tap("unbound", 9, data["approve"])
	tap("unknown", 7, "approve:nope")
	tap("forged", 7, "notification:"+taskID)
	tap("other-chat", 8, data["approve"])
	// Taps from a group, from someone other than the chat owner, or with no message are dropped.
	fake.push(map[string]any{"callback_query": map[string]any{"id": "group", "data": data["approve"], "from": map[string]any{"id": 7}, "message": map[string]any{"message_id": 500, "text": last, "chat": map[string]any{"id": -100, "type": "group"}}}})
	fake.push(map[string]any{"callback_query": map[string]any{"id": "foreign", "data": data["approve"], "from": map[string]any{"id": 5}, "message": map[string]any{"message_id": 500, "text": last, "chat": map[string]any{"id": 7, "type": "private"}}}})
	fake.push(map[string]any{"callback_query": map[string]any{"id": "no-message", "data": data["approve"], "from": map[string]any{"id": 7}}})
	tap("first", 7, data["approve"])
	tap("second", 7, data["approve"])
	// Chat 7 already holds the two notification parts.
	eventually(t, func() bool { return len(fake.sentTo("7")) == 4 })

	mu.Lock()
	if actions != 1 || len(order) < 2 || order[0] != "answer" {
		t.Fatalf("actions=%d order=%v", actions, order)
	}
	mu.Unlock()
	if got := fake.sentTo("7")[2:]; got[0] != "Aprovado." || got[1] != "Este aviso expirou." {
		t.Fatalf("chat 7 replies %q", got)
	}
	if got := fake.sentTo("9"); len(got) != 1 || got[0] != "Vincule sua conta." {
		t.Fatalf("unbound replies %q", got)
	}
	if got := fake.sentTo("8"); len(got) != 1 || got[0] != "Este aviso expirou." {
		t.Fatalf("other chat replies %q", got)
	}
	if got := fake.sentTo("-100"); len(got) != 0 {
		t.Fatalf("group replies %q", got)
	}
	edits := []map[string]any{}
	for _, edit := range fake.sent("editMessageText") {
		if edit["chat_id"] == "7" {
			edits = append(edits, edit)
		}
		if kb := edit["reply_markup"].(map[string]any)["inline_keyboard"].([]any); len(kb) != 0 {
			t.Fatal("edit kept buttons")
		}
	}
	if len(edits) != 2 || !strings.HasSuffix(edits[0]["text"].(string), "Processando…") || !strings.HasSuffix(edits[1]["text"].(string), "\n\nAprovado.") {
		t.Fatalf("edits %d", len(edits))
	}
	if n := len(utf16.Encode([]rune(edits[1]["text"].(string)))); n > 4096 {
		t.Fatalf("edit has %d chars", n)
	}
	var queuedTaps int64
	store.DB.Model(&domain.BotInteraction{}).Where("bot_id = ? AND action <> ? AND action <> ?", bot.ID, "", "notification").Count(&queuedTaps)
	if queuedTaps != 4 {
		t.Fatalf("queued taps %d, want unbound, other-chat, first, second", queuedTaps)
	}
}

func TestStripBotMention(t *testing.T) {
	for in, want := range map[string]string{"/bind@Bot 123": "/bind 123", "/new@Bot": "/new", "/new": "/new", "oi @Bot": "oi @Bot", "/bind@Bot\n123": "/bind\n123"} {
		if got := stripBotMention(in); got != want {
			t.Errorf("%q -> %q", in, got)
		}
	}
	if got := fitTelegram(strings.Repeat("😀", 3000), "\n\nfim"); len([]rune(got)) > 4096 || !strings.HasSuffix(got, "\n\nfim") {
		t.Errorf("fitTelegram length %d", len([]rune(got)))
	}
}
