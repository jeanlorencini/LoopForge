package botinteraction

import (
	"agentflow/internal/domain"
	"context"
	"encoding/json"
	"errors"
)

// transport is the bot app connection that receive and process drive. The
// WeCom websocket and the Telegram poller implement it; the queue, commands,
// binding and permission checks stay in Service.
type transport interface {
	closed() <-chan struct{}
	close()
	// acknowledge tells the sender a queued message was received.
	acknowledge(ctx context.Context, item domain.BotInteraction)
	// progressTurn keeps a model turn visible while it runs.
	progressTurn(ctx context.Context, item domain.BotInteraction) (context.Context, func())
	// sendCard sends a notification with one button per action.
	sendCard(ctx context.Context, item domain.BotInteraction, chatID, taskID string, actions []map[string]any) error
	// actionStarted marks a tapped notification as being handled.
	actionStarted(ctx context.Context, item domain.BotInteraction)
	updateActionCard(ctx context.Context, item domain.BotInteraction, result string, finish bool) error
	// reply sends the final answer of an item and drops its per-item state.
	reply(ctx context.Context, item domain.BotInteraction, chatID, text string) error
}

func (s *socket) closed() <-chan struct{} { return s.done }

func (s *socket) acknowledge(ctx context.Context, item domain.BotInteraction) {
	if item.Action == "" {
		state := s.streamState(item.ID)
		go func() { _ = s.streamWithState(ctx, item, "Recebido, processando…", false, state) }()
	}
}

func (s *socket) sendCard(ctx context.Context, item domain.BotInteraction, chatID, taskID string, _ []map[string]any) error {
	var card map[string]any
	if json.Unmarshal([]byte(item.Quote), &card) != nil {
		return errors.New("Aviso com botões inválido")
	}
	card["task_id"] = taskID
	return s.request(ctx, "aibot_send_msg", "", map[string]any{"chatid": chatID, "msgtype": "template_card", "template_card": card})
}

func (s *socket) actionStarted(context.Context, domain.BotInteraction) {}

func (s *socket) reply(ctx context.Context, item domain.BotInteraction, chatID, text string) error {
	defer s.streams.Delete(item.ID)
	if item.Action == "" && s.stream(ctx, item, text, true) == nil {
		return nil
	}
	return s.send(ctx, chatID, text)
}
