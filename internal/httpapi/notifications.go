package httpapi

import (
	"agentflow/internal/domain"
	"agentflow/internal/notification"
	"agentflow/internal/repo"
	"errors"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// telegramTokenPattern also keeps the token safe to place in the Bot API URL path.
var telegramTokenPattern = regexp.MustCompile(`^(\d+):[A-Za-z0-9_-]+$`)

type notificationHandlers struct {
	*deps
	service *notification.Service
}

func (h *notificationHandlers) access(c *gin.Context, projectID string, write bool) bool {
	project, ok := h.repo.Project(projectID)
	if !ok {
		c.JSON(404, gin.H{"error": "O projeto não existe"})
		return false
	}
	user, authenticated := h.requestUser(c.Request, true)
	if !authenticated || user == nil {
		if !authRequired() {
			return true
		}
		c.JSON(401, gin.H{"error": "Faça login primeiro"})
		return false
	}
	if h.isPlatformAdmin(c.Request) || project.OwnerID == user.ID {
		return true
	}
	for _, member := range project.Members {
		if member.UserID == user.ID && (!write || !strings.EqualFold(strings.TrimSpace(member.Role), "viewer")) {
			return true
		}
	}
	c.JSON(403, gin.H{"error": "Você não tem permissão para gerenciar os avisos deste projeto"})
	return false
}
func (h *notificationHandlers) managed(c *gin.Context, bot domain.NotificationBot) bool {
	if !h.access(c, bot.ProjectID, true) {
		return false
	}
	if !authRequired() || h.isPlatformAdmin(c.Request) {
		return true
	}
	user, ok := h.requestUser(c.Request, true)
	if !ok || user == nil {
		return false
	}
	project, _ := h.repo.Project(bot.ProjectID)
	if bot.OwnerID == user.ID || project.OwnerID == user.ID {
		return true
	}
	for _, m := range project.Members {
		if m.UserID == user.ID && strings.EqualFold(strings.TrimSpace(m.Role), "maintainer") {
			return true
		}
	}
	c.JSON(403, gin.H{"error": "Só quem criou o bot ou um administrador do projeto pode alterar o bot e enviar avisos de teste"})
	return false
}
func (h *notificationHandlers) find(c *gin.Context, write bool) (domain.NotificationBot, bool) {
	bot, err := h.repo.NotificationBot(c.Param("id"))
	if err != nil {
		c.JSON(404, gin.H{"error": "O bot não existe"})
		return bot, false
	}
	if write {
		return bot, h.managed(c, bot)
	}
	return bot, h.access(c, bot.ProjectID, false)
}
func (h *notificationHandlers) bots(c *gin.Context) {
	switch c.Request.Method {
	case http.MethodGet:
		projectID := strings.TrimSpace(c.Query("projectId"))
		if !h.access(c, projectID, false) {
			return
		}
		bots, err := h.repo.NotificationBots(projectID)
		if err != nil {
			respond(c, err)
			return
		}
		c.JSON(200, bots)
	case http.MethodPost:
		h.save(c, nil)
	default:
		c.JSON(405, gin.H{"error": "method not allowed"})
	}
}
func (h *notificationHandlers) bot(c *gin.Context) {
	switch c.Request.Method {
	case http.MethodPut:
		bot, ok := h.find(c, true)
		if ok {
			h.save(c, &bot)
		}
	case http.MethodDelete:
		bot, ok := h.find(c, true)
		if !ok {
			return
		}
		respond(c, h.repo.DeleteNotificationBot(bot.ID))
	default:
		c.JSON(405, gin.H{"error": "method not allowed"})
	}
}
func (h *notificationHandlers) save(c *gin.Context, old *domain.NotificationBot) {
	var req struct {
		ProjectID          string  `json:"projectId"`
		Name               string  `json:"name"`
		BotID              string  `json:"botId"`
		Secret             string  `json:"secret"`
		Enabled            bool    `json:"enabled"`
		Model              *string `json:"model"`
		InteractionEnabled *bool   `json:"interactionEnabled"`
		TargetID           *string `json:"targetId"`
		Provider           string  `json:"provider"`
	}
	if !decode(c, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.BotID = strings.TrimSpace(req.BotID)
	req.Secret = strings.TrimSpace(req.Secret)
	provider := strings.TrimSpace(req.Provider)
	if old != nil {
		provider = old.Provider
	}
	if provider != "" && provider != domain.BotProviderTelegram {
		c.JSON(400, gin.H{"error": "Canal de aviso não suportado"})
		return
	}
	if provider == domain.BotProviderTelegram {
		if req.Secret != "" {
			m := telegramTokenPattern.FindStringSubmatch(req.Secret)
			if m == nil {
				c.JSON(400, gin.H{"error": "Preencha o token do bot do Telegram fornecido pelo @BotFather"})
				return
			}
			req.BotID = domain.TelegramBotIDPrefix + m[1]
		} else if old != nil {
			req.BotID = old.BotID
		}
	}
	if req.Name == "" || len([]rune(req.Name)) > 80 || req.BotID == "" || len(req.BotID) > 191 || len(req.Secret) > 512 {
		c.JSON(400, gin.H{"error": "Preencha nome, Bot ID e Secret válidos"})
		return
	}
	bot := domain.NotificationBot{ID: "bot-" + randomID(), ProjectID: strings.TrimSpace(req.ProjectID), Provider: provider, Name: req.Name, BotID: req.BotID, Enabled: req.Enabled, Health: "unchecked"}
	if old != nil {
		bot = *old
		bot.Name = req.Name
		bot.Enabled = req.Enabled
		if old.BotID != req.BotID {
			if req.Secret == "" {
				c.JSON(400, gin.H{"error": "Ao mudar o Bot ID, preencha também o novo Secret"})
				return
			}
			bot.BotID = req.BotID
			bot.Target = nil
			bot.Health = "unchecked"
			bot.LastCheckedAt = nil
		}
	}
	if req.InteractionEnabled != nil {
		bot.InteractionEnabled = *req.InteractionEnabled
	}
	if req.Model != nil {
		model := strings.TrimSpace(*req.Model)
		if model == "" {
			model = domain.DefaultNotificationReportModel
		}
		// Claude Code ids run on the sender's key or the platform subscription.
		if engine, name := assistantModelEngine(model); !(engine == assistantEngineClaude && name != "") && assistantAllowedModel(model, assistantKnownCatalog()) != model {
			c.JSON(400, gin.H{"error": "Escolha um modelo CodeBuddy ou Claude Code válido"})
			return
		}
		bot.Model = model
	}
	if bot.Model == "" {
		bot.Model = domain.DefaultNotificationReportModel
	}
	if !h.access(c, bot.ProjectID, true) {
		return
	}
	if old == nil {
		if user, ok := h.requestUser(c.Request, true); ok && user != nil {
			bot.OwnerID = user.ID
		}
		if req.Secret == "" {
			c.JSON(400, gin.H{"error": "Um bot novo precisa de Secret"})
			return
		}
	}
	if req.Secret != "" {
		bot.SecretConfigured = true
		bot.Health = "unchecked"
		bot.LastCheckedAt = nil
	}
	// A target is accepted only after refreshing the real provider list. Never trust a posted name/type.
	if bot.InteractionEnabled {
		if bot.OwnerID == 0 {
			c.JSON(400, gin.H{"error": "Faça login para criar um bot com conversa. A plataforma precisa registrar a conta de execução de quem o criou"})
			return
		}
		bots, err := h.repo.InteractiveNotificationBots()
		if err != nil {
			respond(c, err)
			return
		}
		for _, existing := range bots {
			if existing.BotID == bot.BotID && existing.ID != bot.ID && bot.Enabled {
				c.JSON(409, gin.H{"error": "Este bot já tem a conversa ligada em outra configuração. Reutilize a configuração existente."})
				return
			}
		}
		// The conversation lease is per BotID, so a fake token must not claim it.
		if bot.Provider == domain.BotProviderTelegram && bot.Enabled {
			token := req.Secret
			if token == "" {
				var err error
				if token, err = h.repo.BotCredential(bot.ID); err != nil {
					respond(c, err)
					return
				}
			}
			if err := h.service.CheckTelegramToken(c.Request.Context(), bot.BotID, token); err != nil {
				c.JSON(400, gin.H{"error": "Não foi possível confirmar o token no Telegram: " + err.Error()})
				return
			}
		}
	}
	if req.TargetID != nil {
		if *req.TargetID == "" {
			bot.Target = nil
		} else {
			if old == nil || req.Secret != "" || old.BotID != req.BotID {
				c.JSON(400, gin.H{"error": "Salve as credenciais antes de atualizar e escolher o chat de destino"})
				return
			}
			chats, err := h.service.Sessions(c.Request.Context(), bot)
			if err != nil {
				h.providerError(c, err)
				return
			}
			found := false
			for _, chat := range chats {
				if chat.ID == *req.TargetID {
					target := chat
					bot.Target = &target
					found = true
					break
				}
			}
			if !found {
				c.JSON(409, gin.H{"error": "O chat de destino não está mais disponível. Atualize a lista"})
				return
			}
			bot.Health = "ready"
			now := time.Now().UTC()
			bot.LastCheckedAt = &now
		}
	}
	if err := h.repo.SaveNotificationBot(bot, req.Secret); err != nil {
		if errors.Is(err, repo.ErrInteractiveBotAlreadyExists) {
			c.JSON(409, gin.H{"error": err.Error()})
			return
		}
		respond(c, err)
		return
	}
	stored, err := h.repo.NotificationBot(bot.ID)
	if err != nil {
		respond(c, err)
		return
	}
	c.JSON(200, stored)
}
func (h *notificationHandlers) sessions(c *gin.Context) {
	bot, ok := h.find(c, true)
	if !ok {
		return
	}
	chats, err := h.service.Sessions(c.Request.Context(), bot)
	if err != nil {
		h.providerError(c, err)
		return
	}
	query := strings.ToLower(strings.TrimSpace(c.Query("q")))
	filtered := make([]domain.BotChat, 0, len(chats))
	for _, chat := range chats {
		if query == "" || strings.Contains(strings.ToLower(chat.Name), query) {
			filtered = append(filtered, chat)
		}
	}
	c.JSON(200, gin.H{"sessions": filtered, "total": len(chats), "checkedAt": time.Now().UTC()})
}
func (h *notificationHandlers) test(c *gin.Context) {
	bot, ok := h.find(c, true)
	if !ok {
		return
	}
	if bot.Target == nil {
		c.JSON(400, gin.H{"error": "Escolha e salve o chat de destino primeiro"})
		return
	}
	content := "### Teste de notificação\nBot: " + bot.Name + "\nEsta é uma notificação de teste e não representa o status de nenhuma tarefa."
	now := time.Now().UTC()
	item := domain.NotificationDelivery{ID: "test-" + randomID(), ProjectID: bot.ProjectID, BotID: bot.ID, EventType: "test", TargetID: bot.Target.ID, TargetName: bot.Target.Name, Content: content, Status: "sending", Attempts: 1, LeaseToken: randomID(), CreatedAt: now, NextAttemptAt: now}
	until := now.Add(2 * time.Minute)
	item.LeaseUntil = &until
	if err := h.repo.CreateNotificationDelivery(item); err != nil {
		respond(c, err)
		return
	}
	if err := h.service.Send(c.Request.Context(), bot, bot.Target.ID, content); err != nil {
		status := "failed"
		var pe *notification.ProviderError
		if errors.As(err, &pe) {
			switch pe.Code {
			case 850003, 853004:
				status = "awaiting_authorization"
			case 404:
				status = "target_unavailable"
			}
		}
		_ = h.repo.FinishNotification(item, status, err.Error(), now)
		h.providerError(c, err)
		return
	}
	if err := h.repo.FinishNotification(item, "sent", "", now); err != nil {
		respond(c, err)
		return
	}
	c.JSON(200, gin.H{"success": true, "targetName": bot.Target.Name})
}
func (h *notificationHandlers) providerError(c *gin.Context, err error) {
	code := 0
	var pe *notification.ProviderError
	if errors.As(err, &pe) {
		code = pe.Code
	}
	c.JSON(502, gin.H{"error": err.Error(), "code": code})
}
func (h *notificationHandlers) deliveries(c *gin.Context) {
	projectID := strings.TrimSpace(c.Query("projectId"))
	if !h.access(c, projectID, false) {
		return
	}
	items, err := h.repo.NotificationDeliveries(projectID)
	if err != nil {
		respond(c, err)
		return
	}
	c.JSON(200, items)
}
func (h *notificationHandlers) retry(c *gin.Context) {
	projectID := strings.TrimSpace(c.Query("projectId"))
	if !h.access(c, projectID, true) {
		return
	}
	// Only a Bot manager may resend to its configured recipient.
	items, err := h.repo.NotificationDeliveries(projectID)
	if err != nil {
		respond(c, err)
		return
	}
	for _, item := range items {
		if item.ID == c.Param("deliveryId") {
			bot, err := h.repo.NotificationBot(item.BotID)
			if err != nil {
				c.JSON(404, gin.H{"error": "O bot foi apagado"})
				return
			}
			if !h.managed(c, bot) {
				return
			}
			err = h.repo.RetryNotification(projectID, item.ID)
			if errors.Is(err, gorm.ErrRecordNotFound) {
				c.JSON(409, gin.H{"error": "Este registro não pode ser reenviado"})
				return
			}
			respond(c, err)
			return
		}
	}
	c.JSON(404, gin.H{"error": "O registro de envio não existe"})
}

func (h *notificationHandlers) preview(c *gin.Context) {
	var req struct {
		ProjectID  string                     `json:"projectId"`
		Binding    domain.NotificationBinding `json:"binding"`
		SourceType string                     `json:"sourceType"`
	}
	if !decode(c, &req) {
		return
	}
	if !h.access(c, req.ProjectID, false) {
		return
	}
	project, _ := h.repo.Project(req.ProjectID)
	values := map[string]string{"project": project.Name, "workflow": "Fluxo de entrega de requisito", "node": "Implementar a funcionalidade", "status": "Nó concluído", "issue.title": "Melhorar a validação de login", "issue.id": "Requisito de exemplo", "task.name": "Resumo diário de andamento", "input": "Melhorar a validação de login e concluir a verificação", "error": "Erro de exemplo (só em falhas)"}
	if req.Binding.Scope == "run" {
		values["status"] = "SOP concluído"
		values["node"] = "Todos os nós"
	}
	if req.SourceType == "schedule_job" || req.SourceType == "assistant_task" {
		values["status"] = "Tarefa concluída"
	}
	result := "A validação de login foi melhorada e os testes relacionados passaram.\n\n```notification\n{\"title\":\"Andamento da validação de login\",\"summary\":\"Validação melhorada e testes aprovados.\",\"body\":\"Validação melhorada e testes aprovados. Falta a aceitação manual.\"}\n```"
	if req.Binding.Scope == "run" {
		result = domain.BuildWorkflowNotificationResult(domain.Run{WorkflowSnapshot: domain.WorkflowDefinition{Name: "Fluxo de entrega de requisito", Nodes: []domain.WorkflowNode{{ID: "plan", Name: "Análise do requisito"}, {ID: "impl", Name: "Implementação e verificação"}}}, Nodes: map[string]*domain.NodeRun{"plan": {Status: "succeeded", Output: "Critérios de aceite definidos"}, "impl": {Status: "succeeded", Output: result}}})
	}
	content, source, err := domain.RenderNotification(req.Binding, domain.NotificationContext{Values: values, Result: result, Fallback: "Execução de exemplo concluída", SourceType: req.SourceType})
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	link := strings.TrimRight(h.service.PublicURL, "/") + "/#/runs/example"
	content = strings.ReplaceAll(content, domain.NotificationURLMarker, link)
	c.JSON(200, gin.H{"content": content, "contentSource": source, "note": "Prévia de exemplo, nenhuma mensagem é enviada. O conteúdo da IA é gerado na execução real"})
}
