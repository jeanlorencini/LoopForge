package botinteraction

import (
	"agentflow/internal/domain"
	"agentflow/internal/repo"
	"agentflow/internal/telegram"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

type Generate func(context.Context, domain.NotificationBot, domain.BotConversation, []domain.BotInteraction, domain.BotInteraction) (string, error)
type Action func(context.Context, domain.NotificationBot, domain.BotInteraction, domain.BotConversation) (string, error)
type Service struct {
	Store       *repo.Store
	Generate    Generate
	Action      Action
	URL         string
	TelegramURL string
	mu          sync.Mutex
	running     map[string]*connection
	Done        chan struct{}
	start       sync.Once
}
type connection struct {
	bot    domain.NotificationBot
	cancel context.CancelFunc
	done   chan struct{}
	conn   transport
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func New(store *repo.Store) *Service {
	endpoint := strings.TrimSpace(os.Getenv("AGENTFLOW_WECOM_WS_URL"))
	if endpoint == "" {
		endpoint = "wss://openws.work.weixin.qq.com"
	}
	return &Service{Store: store, URL: endpoint, TelegramURL: telegram.BaseURL, running: map[string]*connection{}, Done: make(chan struct{})}
}
func (s *Service) Start(ctx context.Context) {
	s.start.Do(func() {
		go func() {
			ticker := time.NewTicker(3 * time.Second)
			defer ticker.Stop()
			defer func() {
				s.mu.Lock()
				connections := []*connection{}
				for _, c := range s.running {
					c.cancel()
					connections = append(connections, c)
				}
				s.mu.Unlock()
				for _, c := range connections {
					<-c.done
				}
				close(s.Done)
			}()
			nextCleanup := time.Time{}
			for {
				if ctx.Err() != nil {
					return
				}
				now := time.Now().UTC()
				if !now.Before(nextCleanup) {
					if _, err := s.Store.DeleteExpiredBotBindingChallenges(ctx, now); err != nil {
						log.Printf("bot bindings: expired request cleanup failed: %v", err)
						nextCleanup = now.Add(time.Minute)
					} else {
						nextCleanup = now.Add(time.Hour)
					}
				}
				s.reconcile(ctx)
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	})
}
func (s *Service) reconcile(ctx context.Context) {
	bots, err := s.Store.InteractiveNotificationBots()
	if err != nil {
		log.Printf("bot interactions: list unavailable")
		return
	}
	wanted := map[string]domain.NotificationBot{}
	for _, b := range bots {
		wanted[b.ID] = b
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, c := range s.running {
		b, ok := wanted[id]
		if !ok || !b.UpdatedAt.Equal(c.bot.UpdatedAt) {
			c.cancel()
		}
		select {
		case <-c.done:
			delete(s.running, id)
		default:
		}
	}
	for id, b := range wanted {
		if _, exists := s.running[id]; exists {
			continue
		}
		child, cancel := context.WithCancel(ctx)
		c := &connection{bot: b, cancel: cancel, done: make(chan struct{})}
		s.running[id] = c
		go func() { defer close(c.done); s.run(child, c) }()
	}
}
func (s *Service) run(ctx context.Context, c *connection) {
	owner := randomID()
	claimed, err := s.Store.ClaimBotConnection(c.bot.BotID, owner, time.Now().UTC())
	if err != nil || !claimed {
		return
	}
	defer s.Store.ReleaseBotConnection(c.bot.BotID, owner)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := s.Store.RecoverBotInteractions(c.bot.ID); err != nil {
		return
	}
	// Renew independently of model turns; a lost lease cancels every local action.
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				ok, err := s.Store.RenewBotConnection(c.bot.BotID, owner, "", "", time.Now().UTC())
				if err != nil || !ok {
					cancel()
					return
				}
			}
		}
	}()
	backoff := time.Second
	for runCtx.Err() == nil {
		secret, err := s.Store.BotCredential(c.bot.ID)
		if err != nil {
			s.state(c.bot, owner, "error", "Credenciais do bot indisponíveis")
			return
		}
		var t transport
		if c.bot.Provider == domain.BotProviderTelegram {
			t, err = s.pollTelegram(runCtx, c.bot, secret)
		} else {
			var ws *socket
			ws, err = dial(runCtx, s.URL, c.bot.BotID, secret, func(received *socket, f Frame) {
				s.receive(runCtx, c.bot, f, received)
			})
			if err == nil {
				t = ws
			}
		}
		if err == nil {
			s.mu.Lock()
			c.conn = t
			s.mu.Unlock()
			s.state(c.bot, owner, "connected", "")
			err = s.process(runCtx, c.bot, owner, t)
			t.close()
			s.mu.Lock()
			c.conn = nil
			s.mu.Unlock()
			backoff = time.Second
		}
		if runCtx.Err() != nil {
			return
		}
		// A refused getUpdates (bad token, active webhook) is shown as is;
		// the platform never deletes another program's webhook.
		var refused *telegram.Error
		if errors.As(err, &refused) {
			s.state(c.bot, owner, "error", refused.Error())
		} else {
			s.state(c.bot, owner, "reconnecting", "Conexão perdida, reconectando")
		}
		timer := time.NewTimer(backoff)
		select {
		case <-runCtx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}
func (s *Service) state(bot domain.NotificationBot, owner, state, detail string) {
	_, _ = s.Store.RenewBotConnection(bot.BotID, owner, state, detail, time.Now().UTC())
}
func normalizeIncoming(bot domain.NotificationBot, f Frame) (domain.BotInteraction, string, string, bool) {
	var incoming Incoming
	if json.Unmarshal(f.Body, &incoming) != nil || incoming.BotID != bot.BotID || incoming.MessageID == "" || incoming.From.UserID == "" {
		return domain.BotInteraction{}, "", "", false
	}
	// Live WeCom callbacks wrap card parameters in template_card_event;
	// also accept the flat structure used by older official SDK type definitions.
	if incoming.Event.TemplateCardEvent.Key != "" {
		incoming.Event.Key = incoming.Event.TemplateCardEvent.Key
	}
	if incoming.Event.TemplateCardEvent.TaskID != "" {
		incoming.Event.TaskID = incoming.Event.TemplateCardEvent.TaskID
	}
	chat := incoming.ChatID
	kind := incoming.ChatType
	if kind == "" && f.Cmd == "aibot_event_callback" {
		kind = "single"
		if incoming.ChatID != "" {
			kind = "group"
		}
	}
	if kind == "single" {
		chat = incoming.From.UserID
	}
	if chat == "" || (kind != "single" && kind != "group") || len(chat) > 191 || len(incoming.From.UserID) > 191 {
		return domain.BotInteraction{}, "", "", false
	}
	text := incoming.Text.Content
	if incoming.Type == "voice" {
		text = incoming.Voice.Content
	}
	action, delivery := "", ""
	if f.Cmd == "aibot_event_callback" {
		if incoming.Event.Type != "template_card_event" {
			return domain.BotInteraction{}, "", "", false
		}
		action, delivery = incoming.Event.Key, incoming.Event.TaskID
		if prefix, _, found := strings.Cut(delivery, "_"); found {
			delivery = prefix
		}
		if action == "" || action == "handled" || delivery == "" {
			return domain.BotInteraction{}, "", "", false
		}
	} else if incoming.Type != "text" && incoming.Type != "voice" {
		text = unsupportedMessageText
	}
	text = domain.NotificationText(text, 16000)
	if strings.TrimSpace(text) == "" && action == "" {
		return domain.BotInteraction{}, "", "", false
	}
	return domain.BotInteraction{ID: repo.BotMessageID(bot.BotID, incoming.MessageID), BotID: bot.ID, SenderID: incoming.From.UserID, RequestID: f.Headers.RequestID, Content: text, Quote: domain.NotificationText(incoming.Quote.Text.Content, 8000), Action: action, DeliveryID: delivery, CardTaskID: incoming.Event.TaskID}, kind, chat, true
}
func (s *Service) receive(ctx context.Context, bot domain.NotificationBot, f Frame, ws *socket) {
	item, kind, chat, ok := normalizeIncoming(bot, f)
	if !ok {
		if f.Cmd == "aibot_event_callback" {
			var event Incoming
			_ = json.Unmarshal(f.Body, &event)
			log.Printf("wecom ignored event type=%s bot_matches=%t message_id=%t sender=%t chat_type=%s action=%s task_id=%t", event.Event.Type, event.BotID == bot.BotID, event.MessageID != "", event.From.UserID != "", event.ChatType, event.Event.Key, event.Event.TaskID != "")
		}
		return
	}
	// Single-chat callbacks identify the actor by internal userid, while CLI
	// discovery targets use openid. Resolve only an exact, server-issued card
	// task to its frozen recipient; group callbacks still require matching chatid.
	if item.Action != "" && kind == "single" {
		delivery, e := s.Store.NotificationDelivery(item.DeliveryID)
		card, cardErr := s.Store.BotInteraction(repo.BotMessageID(bot.ID, "notification:"+item.DeliveryID))
		if e == nil && cardErr == nil && delivery.BotID == bot.ID && delivery.ProjectID == bot.ProjectID && delivery.TargetType == "single" && card.CardTaskID == item.CardTaskID {
			chat = delivery.TargetID
		}
	}
	_ = s.accept(ctx, item, kind, chat, ws)
}

func (s *Service) accept(ctx context.Context, item domain.BotInteraction, kind, chat string, t transport) error {
	inserted, err := s.Store.EnqueueBotInteraction(item, kind, chat)
	if err != nil {
		log.Printf("bot event enqueue failed action=%s", item.Action)
		return err
	}
	// Card operations are direct engine controls. Reply with the actual result
	// within the callback window rather than consuming it with a receipt-only update.
	if inserted {
		t.acknowledge(ctx, item)
	}
	return nil
}

const unsupportedMessageText = "[Tipo de mensagem não suportado. Envie texto.]"

var ErrCardActionDeferred = errors.New("Aguardando a confirmação da conta ou a permissão no projeto")

func newConversationCommand(content string) bool {
	text := strings.TrimSpace(content)
	if strings.HasPrefix(text, "@") {
		if _, rest, ok := strings.Cut(text, " "); ok {
			text = strings.TrimSpace(rest)
		}
		if _, rest, ok := strings.Cut(text, "\n"); ok {
			text = strings.TrimSpace(rest)
		}
	}
	return text == "/new" || text == "开启新会话" || text == "新会话"
}
func (s *Service) process(ctx context.Context, bot domain.NotificationBot, owner string, t transport) error {
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.closed():
			return errors.New("Conexão perdida")
		case <-ticker.C:
		}
		item, err := s.Store.ClaimBotInteraction(bot.ID, bot.BotID, owner)
		if err != nil {
			return err
		}
		if item == nil {
			continue
		}
		conv, err := s.Store.BotConversation(item.ConversationID)
		if err != nil {
			return err
		}
		if item.Action == "notification" {
			delivery, lookupErr := s.Store.NotificationDelivery(item.DeliveryID)
			var actions []map[string]any
			if lookupErr == nil {
				actions = ActionsForDelivery(s.Store, delivery)
			}
			if len(actions) == 0 {
				if err = s.Store.ExpireBotCard(item.ID); err != nil {
					return err
				}
				continue
			}
			if item.Status != "ready" {
				if err = s.Store.SaveBotReply(*item, "", ""); err != nil {
					return err
				}
			}
			// A task_id may not be reused by the same Bot across sent cards.
			// Keep the delivery prefix for callback routing; suffix each send attempt.
			// Telegram callback_data holds at most 64 bytes, so it carries only the suffix.
			taskID := randomID()[:12]
			if bot.Provider != domain.BotProviderTelegram {
				taskID = item.DeliveryID + "_" + taskID
			}
			if err = s.Store.SetBotCardTaskID(item.ID, taskID); err != nil {
				return err
			}
			if err = t.sendCard(ctx, *item, conv.ChatID, taskID, actions); err != nil {
				var business *ReplyError
				if errors.As(err, &business) {
					if err = s.Store.FailBotReply(item.ID, business.Error()); err != nil {
						return err
					}
					continue
				}
				return err
			}
			if err = s.Store.MarkBotReplySent(item.ID); err != nil {
				return err
			}
			continue
		}
		if item.Action == "notification_details" {
			delivery, lookupErr := s.Store.NotificationDelivery(item.DeliveryID)
			if lookupErr != nil || len(ActionsForDelivery(s.Store, delivery)) == 0 {
				if err = s.Store.ExpireBotCard(item.ID); err != nil {
					return err
				}
				continue
			}
		}
		if item.Status != "ready" {
			reply, detail := "", ""
			if item.Action == "notification_details" {
				reply = item.Content
			} else if time.Since(item.CreatedAt) > 10*time.Minute {
				reply = "Esta mensagem esperou demais na fila e não foi executada. Envie o pedido de novo."
			} else if newConversationCommand(item.Content) {
				err = s.Store.NewBotConversation(*item)
				reply = "Nova conversa iniciada. As próximas mensagens partem de um contexto novo, e o histórico continua salvo."
			} else if item.Content == unsupportedMessageText {
				reply = item.Content
			} else {
				turnCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				var stopProgress func()
				if item.Action == "" {
					turnCtx, stopProgress = t.progressTurn(turnCtx, *item)
				}
				if item.Action != "" {
					if s.Action == nil {
						err = errors.New("Serviço de ações indisponível")
					} else {
						tapped := *item
						actionCtx := context.WithValue(turnCtx, actionStartedKey{}, func() { t.actionStarted(turnCtx, tapped) })
						reply, err = s.Action(actionCtx, bot, *item, conv)
					}
				} else {
					history, historyErr := s.Store.BotHistory(conv.SessionID)
					err = historyErr
					if err == nil {
						if s.Generate == nil {
							err = errors.New("Serviço do assistente indisponível")
						} else {
							reply, err = s.Generate(turnCtx, bot, conv, history, *item)
						}
					}
				}
				if stopProgress != nil {
					stopProgress()
				}
				cancel()
			}
			if errors.Is(err, ErrCardActionDeferred) {
				detail = ErrCardActionDeferred.Error()
			} else if err != nil {
				detail = "Falha no processamento"
				reply = "O pedido não foi concluído. Tente de novo mais tarde ou confira o status na plataforma."
				if errors.Is(err, context.DeadlineExceeded) {
					detail = "Tempo esgotado ao enviar a resposta do modelo"
					reply = "O pedido excedeu o tempo e a resposta final não foi enviada. Envie o pedido de novo. Se ele altera algo, confira antes o status na plataforma para não repetir a ação."
				}
			}
			if strings.TrimSpace(reply) == "" {
				detail = "O assistente não enviou resposta"
				reply = "Não houve resposta válida desta vez. Tente de novo mais tarde."
			}
			reply = domain.NotificationText(reply, 16000)
			if reply, detail, err = s.saveReply(*item, reply, detail); err != nil {
				return err
			}
			item.Reply = reply
			item.LastError = detail
		}
		if item.Action != "" && item.Action != "notification_details" && item.CardTaskID != "" && item.LastError != ErrCardActionDeferred.Error() {
			if updateErr := t.updateActionCard(ctx, *item, item.Reply, true); updateErr != nil {
				code := 0
				var business *ReplyError
				if errors.As(updateErr, &business) {
					code = business.Code
				}
				log.Printf("bot action card update failed action=%s code=%d", item.Action, code)
			}
		}
		if err = t.reply(ctx, *item, conv.ChatID, item.Reply); err != nil {
			var business *ReplyError
			if errors.As(err, &business) {
				if err = s.Store.FailBotReply(item.ID, business.Error()); err != nil {
					return err
				}
				continue
			}
			return err
		}
		if err = s.Store.MarkBotReplySent(item.ID); err != nil {
			return err
		}
	}
}
