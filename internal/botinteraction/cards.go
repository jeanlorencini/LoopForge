package botinteraction

import (
	"agentflow/internal/domain"
	"agentflow/internal/repo"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"
)

var ErrCardExpired = errors.New("Este nó não espera mais por esta ação")

func ActionsForDelivery(store *repo.Store, item domain.NotificationDelivery) []map[string]any {
	run, ok := store.RunState(item.RunID)
	if !ok {
		return nil
	}
	node := run.Nodes[item.NodeID]
	if node == nil || node.Attempt != item.Attempt || node.ReworkCount != item.ReworkCount || (item.EventType == "waiting" && node.Status != "waiting_approval") || (item.EventType == "failed" && node.Status != "failed") {
		return nil
	}
	actions := []map[string]any{}
	button := func(text, key string) {
		style := 1
		if key == "pass" || key == "decline" || key == "reject" || (key == "retry" && node.Status == "waiting_approval") {
			style = 4
		}
		actions = append(actions, map[string]any{"text": text, "key": key, "style": style})
	}
	switch node.Status {
	case "waiting_approval":
		if domain.TerminalRunStatus(run.Status) {
			return nil
		}
		if strings.Contains(node.Activity, "用户暂停") {
			button("Continuar", "resume")
		} else if strings.Contains(node.Activity, "轮次用尽") || strings.Contains(node.Activity, "是否重试") || strings.Contains(strings.ToLower(node.Error), "max turns") {
			button("Repetir", "retry")
			button("Não repetir", "decline")
		} else {
			button("Aprovar", "approve")
			if run.Status != "running" {
				button("Repetir", "retry")
			}
			if n, ok := snapshotNode(run, item.NodeID); ok && n.RejectTo != "" {
				button("Retrabalho", "reject")
			}
		}
	case "failed":
		button("Repetir", "retry")
		if n, ok := snapshotNode(run, item.NodeID); ok && n.Type == "script" {
			button("Liberar manualmente", "pass")
		}
	default:
		return nil
	}
	return actions
}
func handledCard(id, text string) map[string]any {
	return map[string]any{"card_type": "button_interaction", "task_id": id, "main_title": map[string]string{"title": text}, "button_list": []map[string]any{{"text": "Recebido", "key": "handled", "style": 3}}}
}
func (s *Service) QueueNotificationCard(ctx context.Context, bot domain.NotificationBot, item domain.NotificationDelivery, content string, detailsURL ...string) error {
	actions := ActionsForDelivery(s.Store, item)
	if len(actions) == 0 {
		return ErrCardExpired
	}
	run, ok := s.Store.RunState(item.RunID)
	if !ok {
		return ErrCardExpired
	}
	quote := ""
	if bot.Provider == domain.BotProviderTelegram {
		// The Telegram notification is the whole text plus buttons, so it needs
		// neither the WeCom card nor its companion details message.
		content = telegramCardText(run, item, actions, content, detailsURL...)
	} else {
		card := notificationCard(run, item, actions, content)
		if detail := notificationCardDetails(run, item, content, detailsURL...); detail != "" {
			_, err := s.Store.EnqueueBotInteraction(domain.BotInteraction{ID: repo.BotMessageID(bot.ID, "notification-details:"+item.ID), BotID: bot.ID, Action: "notification_details", DeliveryID: item.ID, Content: detail}, item.TargetType, item.TargetID)
			if err != nil {
				return err
			}
		}
		if len(detailsURL) > 0 && detailsURL[0] != "" {
			card["jump_list"] = []map[string]any{{"type": 1, "title": "Ver detalhes", "url": detailsURL[0]}}
		}
		data, _ := json.Marshal(card)
		quote = string(data)
	}
	id := repo.BotMessageID(bot.ID, "notification:"+item.ID)
	_, err := s.Store.EnqueueBotInteraction(domain.BotInteraction{ID: id, BotID: bot.ID, Action: "notification", DeliveryID: item.ID, Content: content, Quote: quote}, item.TargetType, item.TargetID)
	if err != nil {
		return err
	}
	if err = s.Store.RequeueBotCard(id); err != nil {
		return err
	}
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		record, err := s.Store.BotInteraction(id)
		if err != nil {
			return err
		}
		if record.Status == "reply_failed" {
			return errors.New("Falha ao enviar o aviso com botões. Confira o chat e a autorização do bot.")
		}
		if record.Status == "replied" {
			if record.LastError != "" {
				return ErrCardExpired
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func (s *Service) NotificationCardTask(bot domain.NotificationBot, item domain.BotInteraction, conv domain.BotConversation) (domain.NotificationDelivery, error) {
	delivery, err := s.Store.NotificationDelivery(item.DeliveryID)
	if err != nil || delivery.BotID != bot.ID || delivery.ProjectID != bot.ProjectID {
		return delivery, ErrCardExpired
	}
	if item.CardTaskID != "" {
		card, cardErr := s.Store.BotInteraction(repo.BotMessageID(bot.ID, "notification:"+delivery.ID))
		if cardErr != nil || card.CardTaskID != item.CardTaskID {
			return delivery, ErrCardExpired
		}
	}
	if delivery.TargetID != conv.ChatID || delivery.TargetType != conv.ChatType || time.Since(delivery.CreatedAt) > 24*time.Hour {
		return delivery, ErrCardExpired
	}
	if delivery.Status != "sent" && delivery.Status != "sending" && delivery.Status != "pending" {
		return delivery, ErrCardExpired
	}
	allowed := ActionsForDelivery(s.Store, delivery)
	for _, a := range allowed {
		if a["key"] == item.Action {
			return delivery, nil
		}
	}
	return delivery, ErrCardExpired
}
func actionLabel(action string) string {
	return map[string]string{"approve": "Aprovar e continuar", "retry": "Repetir o nó", "pass": "Liberar manualmente"}[strings.TrimSpace(action)]
}

// Card text fields are plain text. Keep the reason visible above context rows,
// instead of embedding a long Markdown notification in the clipped subtitle.
func notificationCard(run domain.Run, item domain.NotificationDelivery, actions []map[string]any, content string) map[string]any {
	reason, required := "", ""
	node := run.Nodes[item.NodeID]
	if node != nil {
		if node.GateResult != nil {
			reason = node.GateResult.Summary
			if strings.TrimSpace(reason) == "" {
				reason = node.GateResult.Reason
			}
			required = node.GateResult.ActionRequired
		}
		if strings.TrimSpace(reason) == "" {
			reason = node.Error
		}
	}
	if strings.TrimSpace(reason) == "" {
		for _, line := range strings.Split(content, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "Requisito:") || strings.HasPrefix(line, "SOP:") || strings.HasPrefix(line, "Nó:") || strings.HasPrefix(line, "Detalhes:") {
				continue
			}
			reason = line
			break
		}
	}
	if strings.TrimSpace(reason) == "" {
		if item.EventType == "waiting" {
			reason = "O fluxo pede confirmação manual aqui. Confirme para continuar."
		} else {
			reason = "A execução do nó falhou. Veja os detalhes da execução."
		}
	}
	// 8 + 70 + 11 + 23 runes fit the 112-rune subtitle.
	body := "Motivo: " + cardText(reason, 70)
	if required != "" {
		body += "\nPendente: " + cardText(required, 23)
	}
	issue := notificationCardIssue(content)
	title := issue
	if title == domain.NotificationNoIssue {
		title = run.WorkflowSnapshot.Name
	}
	rows := []map[string]string{
		{"keyname": "Requisito", "value": cardText(issue, 26)},
		{"keyname": "Nó", "value": cardText(item.SourceName, 26)},
		{"keyname": "SOP", "value": cardText(run.WorkflowSnapshot.Name, 26)},
	}
	for _, action := range actions {
		if action["key"] == "reject" {
			if target := cardReworkTarget(run, item.NodeID); target != "" {
				rows = append(rows, map[string]string{"keyname": "Retrabalho para", "value": cardText(target, 26)})
			}
			break
		}
	}
	buttons, menu := notificationCardActions(actions)
	card := map[string]any{"card_type": "button_interaction", "task_id": item.ID,
		"source":         map[string]any{"desc": map[string]string{"waiting": "Aguardando confirmação", "failed": "Falhou"}[item.EventType], "desc_color": 2},
		"main_title":     map[string]string{"title": cardText(title, 26), "desc": "Nó: " + cardText(item.SourceName, 26)},
		"sub_title_text": body, "horizontal_content_list": rows, "button_list": buttons}
	if menu != nil {
		card["action_menu"] = menu
	}
	return card
}

// telegramCardText adds to the notification what the WeCom card shows in its
// own fields: the rework target and the details link.
func telegramCardText(run domain.Run, item domain.NotificationDelivery, actions []map[string]any, content string, detailsURL ...string) string {
	for _, action := range actions {
		if action["key"] == "reject" {
			if target := cardReworkTarget(run, item.NodeID); target != "" {
				content += "\nRetrabalho volta para: " + target
			}
			break
		}
	}
	if len(detailsURL) > 0 && detailsURL[0] != "" && !strings.Contains(content, detailsURL[0]) {
		content += "\nDetalhes: " + detailsURL[0]
	}
	return content
}
func notificationCardIssue(content string) string {
	for _, line := range strings.Split(content, "\n") {
		if issue, ok := strings.CutPrefix(strings.TrimSpace(line), "Requisito: "); ok && strings.TrimSpace(issue) != "" {
			return strings.TrimSpace(issue)
		}
	}
	return domain.NotificationNoIssue
}

// Keep long evidence intact in a companion Markdown message; the enterprise
// card fields are designed for short summaries and do not support expansion.
func notificationCardDetails(run domain.Run, item domain.NotificationDelivery, content string, detailsURL ...string) string {
	node := run.Nodes[item.NodeID]
	if node == nil {
		return ""
	}
	reason, required := node.Error, ""
	findings := []domain.GateFinding{}
	if node.GateResult != nil {
		if strings.TrimSpace(node.GateResult.Reason) != "" {
			reason = node.GateResult.Reason
		}
		required = node.GateResult.ActionRequired
		findings = node.GateResult.Findings
	}
	if len([]rune(reason)) <= 70 && len([]rune(required)) <= 23 && len(findings) == 0 {
		return ""
	}
	text := "### " + notificationCardIssue(content) + "\nNó: " + item.SourceName + "\nSOP: " + run.WorkflowSnapshot.Name + "\n\n**Motivo**\n" + reason
	if required != "" {
		text += "\n\n**O que fazer**\n" + required
	}
	for _, finding := range findings {
		text += "\n\n- " + finding.Title + "\n" + finding.Evidence
	}
	text += "\n\nUse o card de ações enviado a seguir."
	footer := ""
	if len(detailsURL) > 0 {
		if link, err := url.Parse(detailsURL[0]); err == nil && (link.Scheme == "http" || link.Scheme == "https") && link.Host != "" {
			footer = "\n\n[Ver detalhes da execução](" + link.String() + ")"
		}
	}
	return domain.NotificationText(text, 16000-len(footer)) + footer
}

func cardText(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	text = strings.NewReplacer("**", "", "`", "").Replace(text)
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return text
}

func (s *socket) updateActionCard(ctx context.Context, item domain.BotInteraction, result string, finish bool) error {
	state := s.streamState(item.ID)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.finished {
		return nil
	}
	card := handledCard(item.CardTaskID, "Processando, aguarde")
	if finish {
		title := "Ação não concluída"
		if strings.Contains(result, "Ação concluída") {
			title = "Ação concluída"
		} else if strings.Contains(result, "expirou") {
			title = "Aviso expirado"
		}
		card["main_title"] = map[string]string{"title": title, "desc": cardText(result, 30)}
		card["sub_title_text"] = cardText(result, 112)
		card["button_list"] = []map[string]any{{"text": title, "key": "handled", "style": 4}}
	}
	err := s.request(ctx, "aibot_respond_update_msg", item.RequestID, map[string]any{"response_type": "update_template_card", "template_card": card})
	if err == nil {
		state.finished = finish
	}
	return err
}

// Keep every available operation visible. Three-column cards need short
// labels; the card's context row explicitly identifies the rework target.
func notificationCardActions(actions []map[string]any) ([]map[string]any, map[string]any) {
	labels := map[string]string{"approve": "Aprovar e continuar", "retry": "Repetir o nó", "reject": "Retrabalho", "pass": "Liberar manualmente", "resume": "Continuar", "decline": "Não repetir"}
	if len(actions) > 2 {
		labels = map[string]string{"approve": "Aprovar", "retry": "Repetir", "reject": "Retrabalho", "pass": "Liberar", "resume": "Continuar", "decline": "Não repetir"}
	}
	buttons := []map[string]any{}
	for _, action := range actions {
		key, _ := action["key"].(string)
		text := labels[key]
		if text == "" {
			text, _ = action["text"].(string)
		}
		button := map[string]any{}
		for k, v := range action {
			button[k] = v
		}
		button["text"] = text
		buttons = append(buttons, button)
	}
	return buttons, nil
}
func cardReworkTarget(run domain.Run, nodeID string) string {
	node, ok := snapshotNode(run, nodeID)
	if !ok || node.RejectTo == "" {
		return ""
	}
	if target, ok := snapshotNode(run, node.RejectTo); ok {
		return target.Name
	}
	return ""
}

// snapshotNode returns the definition of node id frozen in the run's workflow
// snapshot. Node IDs are unique per workflow (enforced on save), so the first
// match is the only one.
func snapshotNode(run domain.Run, id string) (domain.WorkflowNode, bool) {
	for _, node := range run.WorkflowSnapshot.Nodes {
		if node.ID == id {
			return node, true
		}
	}
	return domain.WorkflowNode{}, false
}
