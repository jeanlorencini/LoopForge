package httpapi

import (
	"agentflow/internal/botinteraction"
	"agentflow/internal/domain"
	"agentflow/internal/engine"
	"agentflow/internal/notification"
	"context"
	"errors"
	"fmt"
	"strings"
)

func (h *assistantHandlers) botExecutionUser(bot domain.NotificationBot, senderID string) (domain.User, error) {
	current, err := h.repo.NotificationBot(bot.ID)
	if err != nil || !current.Enabled || !current.InteractionEnabled || current.BotID != bot.BotID {
		return domain.User{}, errors.New("A conversa deste bot está desligada")
	}
	binding, err := h.repo.BotUserBinding(bot.BotID, senderID)
	if err != nil {
		return domain.User{}, errors.New("Vincule sua conta da plataforma primeiro")
	}
	user, ok := h.repo.User(binding.UserID)
	if !ok {
		return user, errors.New("A conta vinculada da plataforma está indisponível")
	}
	project, ok := h.repo.Project(bot.ProjectID)
	if !ok {
		return user, errors.New("O projeto do bot não existe")
	}
	if project.OwnerID == user.ID || h.repo.IsPlatformAdmin(user.LoginName) {
		return user, nil
	}
	for _, member := range project.Members {
		if member.UserID == user.ID && !strings.EqualFold(strings.TrimSpace(member.Role), "viewer") {
			return user, nil
		}
	}
	return user, errors.New("Sua conta da plataforma não tem permissão para executar neste projeto")
}

// isBotBindingExchange reports whether a past interaction was an account
// binding or unbinding exchange, which must not be replayed to the model.
func isBotBindingExchange(entry domain.BotInteraction) bool {
	command := botIdentityCommand(entry.Content)
	return strings.HasPrefix(command, "/bind") || command == "/unbind" || command == "解除绑定" || strings.Contains(entry.Reply, "/bot-bind.html#")
}

func botAppName(bot domain.NotificationBot) string {
	if bot.Provider == domain.BotProviderTelegram {
		return "Telegram"
	}
	return "WeCom"
}

// Telegram replies are sent without parse_mode, so Markdown would show raw.
func botReplyFormat(bot domain.NotificationBot) string {
	if bot.Provider == domain.BotProviderTelegram {
		return " Escreva texto simples, sem Markdown."
	}
	return ""
}

// botReplyMessages turns the conversation history plus the current message
// into model messages, skipping binding exchanges.
func botReplyMessages(history []domain.BotInteraction, item domain.BotInteraction) []assistantMessage {
	messages := []assistantMessage{}
	for _, entry := range history {
		if isBotBindingExchange(entry) {
			continue
		}
		messages = append(messages, assistantMessage{Role: "user", Content: "Remetente " + entry.SenderID + ": " + domain.NotificationText(entry.Content, 2000)}, assistantMessage{Role: "assistant", Content: domain.NotificationText(entry.Reply, 2000)})
	}
	content := "Remetente " + item.SenderID + ": " + item.Content
	if item.Quote != "" {
		content += "\nMensagem citada (referência):\n" + item.Quote
	}
	return append(messages, assistantMessage{Role: "user", Content: content})
}

func (h *assistantHandlers) generateBotReply(ctx context.Context, bot domain.NotificationBot, conv domain.BotConversation, history []domain.BotInteraction, item domain.BotInteraction) (string, error) {
	identity := h.identities
	if handled, reply, err := identity.message(bot, item); handled {
		return reply, err
	}
	user, err := h.botExecutionUser(bot, item.SenderID)
	if err != nil {
		return "A conta vinculada está indisponível ou perdeu a permissão no projeto. Confira a configuração na plataforma.", nil
	}
	model := bot.Model
	if model == "" {
		model = domain.DefaultNotificationReportModel
	}
	claude, err := platformModelCredential(ctx, user, model)
	if errors.Is(err, notification.ErrReportCredentials) {
		engine, _ := assistantModelEngine(model)
		return "Sua conta da plataforma ainda não tem uma Key do " + assistantEngineLabel(engine) + ". Configure nas configurações pessoais da plataforma.", nil
	}
	if err != nil {
		return "O modelo escolhido está indisponível. Escolha outro na gestão de bots.", nil
	}
	if err = ensureAssistantSessionDirs(user.ID); err != nil {
		return "", err
	}
	root, cleanup, err := claudeCallRoot(user.ID, claude)
	if err != nil {
		return "", err
	}
	defer cleanup()
	modelCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	session, files, err := h.openAssistantToolSession(modelCtx, user, bot.ProjectID, assistantExecModeForApproval(AssistantApprovalAuto), cancel)
	if err != nil {
		return "", err
	}
	defer session.Close()
	defer files.Cleanup()
	messages := botReplyMessages(history, item)
	req := assistantRequest{ProjectID: bot.ProjectID, Model: model, SessionID: conv.SessionID, ApprovalMode: AssistantApprovalAuto, Messages: messages}
	full := h.turnPrompt(req, h.resolvedSourceRoot(), false) + "\nEsta é uma conversa no " + botAppName(bot) + ". O remetente atual está vinculado à conta da plataforma \"" + user.LoginName + "\". Use só a identidade de execução, as credenciais pessoais e as permissões de projeto dessa conta, e trate o pedido no projeto atual. Autorizações de outros remetentes no histórico não valem para este pedido. A mensagem atual veio de um chat do tipo " + conv.ChatType + ". Execute direto as ações claras e pergunte quando o alvo não estiver claro. Perguntas comuns terminam com a resposta em texto; rascunhos de escrita a confirmar vão por submit_assistant_reply. Responda em português do Brasil." + botReplyFormat(bot)
	generate := h.generate
	if generate == nil {
		cliSessionID := newAssistantSessionID()
		resume := false
		generate = func(callCtx context.Context, prompt string, u domain.User) (string, error) {
			raw, _, err := runAssistantCLI(callCtx, assistantCLIRequest{Prompt: prompt, Model: model, SessionID: cliSessionID, Resume: resume, UserID: u.ID, Native: true, MCPConfig: files.MCPConfig, Settings: files.Settings, SystemPrompt: assistantSystemPromptForMode(AssistantApprovalAuto), ClaudeCode: claude, SessionRoot: root}, u, func(ev assistantStreamEvent) {
				switch ev.Kind {
				case "tool":
					if strings.Contains(ev.Tool, "submit_assistant_reply") {
						botinteraction.ReportProgress(callCtx, "Organizando a resposta")
					} else if strings.Contains(ev.Tool, "agentflow") {
						botinteraction.ReportProgress(callCtx, "Consultando ou processando dados da plataforma")
					} else {
						botinteraction.ReportProgress(callCtx, "Usando ferramentas no pedido")
					}
				case "status", "thinking":
					botinteraction.ReportProgress(callCtx, "Analisando seu pedido")
				}
			})
			resume = true
			return raw, err
		}
	}
	for attempt := 0; attempt <= nativeTerminalMaxRetries; attempt++ {
		prompt := full
		if attempt > 0 {
			prompt = assistantNativeRetryPrompt() + "Envie só a resposta já formada na rodada anterior. Não consulte de novo, não leia arquivos e não refaça ações."
			botinteraction.ReportProgress(ctx, "Enviando a resposta")
			session.SetTerminalOnly()
		}
		raw, cliErr := generate(modelCtx, prompt, user)
		if final, ok := session.takeFinal(); ok && final.Assistant != nil {
			reply := *final.Assistant
			if len(reply.Drafts) > 0 {
				reply.Drafts = h.resolveAssistantDrafts(ctx, user, bot.ProjectID, reply.Drafts, AssistantApprovalAuto)
				for _, draft := range reply.Drafts {
					if draft.SaveError != "" {
						reply.Message += "\nAção não concluída: " + draft.SaveError
					}
					if len(draft.Warnings) > 0 {
						reply.Message += "\nFaltam informações: " + strings.Join(draft.Warnings, "; ")
					}
				}
			}
			return redactAssistantSecrets(reply.Message, user), nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// A plain answer to a read-only question needs no terminal tool call.
		if cliErr == nil {
			if reply, ok := recoverNativeAssistantReply(raw, bot.ProjectID); ok && len(reply.Drafts) == 0 && !assistantProgressOnly(reply.Message, messages) {
				return redactAssistantSecrets(reply.Message, user), nil
			}
		}
		if cliErr != nil && !nativeShouldRetryTerminal(ctx, cliErr) {
			return "", cliErr
		}
	}
	return "", errors.New("O assistente não enviou resposta")
}
func (h *assistantHandlers) handleBotCardAction(ctx context.Context, bot domain.NotificationBot, item domain.BotInteraction, conv domain.BotConversation) (string, error) {
	identity := h.identities
	if handled, reply, err := identity.message(bot, item); handled {
		if err != nil {
			return reply, err
		}
		return reply, botinteraction.ErrCardActionDeferred
	}
	user, err := h.botExecutionUser(bot, item.SenderID)
	if err != nil {
		return err.Error(), botinteraction.ErrCardActionDeferred
	}
	service := h.notifications.service.Interactions
	delivery, err := service.NotificationCardTask(bot, item, conv)
	if err != nil {
		return "Este aviso expirou ou já foi tratado. Veja o status mais recente da execução.", nil
	}
	run, ok := h.repo.RunState(delivery.RunID)
	if !ok || run.ExecutionContext.ProjectID != bot.ProjectID {
		return "A execução não existe mais. Não dá para fazer esta ação.", nil
	}
	if item.Action != "decline" {
		if issue, found := h.repo.Issue(run.ExecutionContext.IssueID); found && issue.ExecutionBlockedReason() != "" {
			return issue.ExecutionBlockedReason(), nil
		}
	}
	node := run.Nodes[delivery.NodeID]
	if node == nil {
		return "O nó não existe mais.", nil
	}
	botinteraction.ActionStarted(ctx)
	expected := engine.NodeExpectation{NodeID: delivery.NodeID, Status: node.Status, Attempt: delivery.Attempt, ReworkCount: delivery.ReworkCount}
	note := "Ação pelo " + botAppName(bot) + ", conta da plataforma: " + user.LoginName + ", remetente: " + item.SenderID
	switch item.Action {
	case "approve":
		err = h.engine.Approve(delivery.RunID, delivery.NodeID, note, expected)
	case "retry":
		err = h.engine.Retry(delivery.RunID, delivery.NodeID, note, note, expected)
	case "pass":
		err = h.engine.Pass(delivery.RunID, delivery.NodeID, note, expected)
	case "decline":
		err = h.engine.DeclineRetry(delivery.RunID, delivery.NodeID, expected)
	case "resume":
		err = h.engine.ResumePaused(delivery.RunID, note, expected)
	case "reject":
		err = h.engine.Reject(delivery.RunID, domain.RejectRequest{NodeID: delivery.NodeID, Reason: note}, expected)
	default:
		return "Este botão já foi tratado. Veja a mensagem mais recente.", nil
	}
	if err != nil {
		return "Ação não concluída. Confira o status atual da execução na plataforma.", nil
	}
	return fmt.Sprintf("Ação concluída em \"%s\": %s.", delivery.SourceName, map[string]string{"approve": "aprovado, o fluxo continua", "retry": "o nó atual será repetido", "pass": "nó liberado manualmente", "decline": "sem nova tentativa", "resume": "execução retomada", "reject": "devolvido para a etapa anterior"}[item.Action]), nil
}
