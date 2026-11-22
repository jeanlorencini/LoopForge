package httpapi

import (
	"agentflow/internal/botinteraction"
	"agentflow/internal/domain"
	"agentflow/internal/engine"
	"agentflow/internal/notification"
	"agentflow/internal/repo"
	"agentflow/internal/tracker"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func botFixture(t *testing.T) (*repo.Store, *assistantHandlers, domain.NotificationBot) {
	t.Helper()
	s, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	t.Setenv("AGENTFLOW_ASSISTANT_SESSION_DIR", t.TempDir())
	s.PutUser(domain.User{ID: 42, LoginName: "bot-owner", CodeBuddyAPIKey: "owner-key", CodeBuddyInternetEnvironment: "ioa"})
	s.PutUser(domain.User{ID: 43, LoginName: "other-user", CodeBuddyAPIKey: "other-key", CodeBuddyInternetEnvironment: "ioa"})
	s.PutProject(domain.Project{ID: "p", OwnerID: 42, Members: []domain.ProjectMember{{UserID: 43, Role: "member"}}})
	bot := domain.NotificationBot{ID: "b", BotID: "external", ProjectID: "p", OwnerID: 42, Enabled: true, InteractionEnabled: true, Model: domain.DefaultNotificationReportModel}
	if err = s.SaveNotificationBot(bot, "secret"); err != nil {
		t.Fatal(err)
	}
	e := engine.New(s, nil)
	e.ConfigureExecution("api", "test", time.Minute, false)
	d := &deps{repo: s, engine: e}
	h := &assistantHandlers{deps: d, notifications: &notificationHandlers{deps: d, service: notification.New(s)}}
	h.notifications.service.PublicURL = "http://platform.example"
	h.identities = &botIdentityHandlers{deps: d, publicURL: h.notifications.service.PublicURL}
	bindBotTestUser(t, s, bot, "new-wecom-user", 43)
	bindBotTestUser(t, s, bot, "bound-member", 42)
	return s, h, bot
}
func TestBotUsesBoundSenderIdentityAutoModeAndSharedChatContext(t *testing.T) {
	s, h, bot := botFixture(t)
	h.generate = func(ctx context.Context, prompt string, user domain.User) (string, error) {
		if user.ID != 43 || user.CodeBuddyAPIKey != "other-key" || user.CodeBuddyInternetEnvironment != "ioa" {
			t.Fatal("wrong credentials")
		}
		if !strings.Contains(prompt, "当前审批模式为「完全权限」") || !strings.Contains(prompt, "Remetente new-wecom-user") || !strings.Contains(prompt, "历史回复") || !strings.Contains(prompt, "conversa no WeCom") || !strings.Contains(prompt, "Responda em português do Brasil.") || strings.Contains(prompt, "sem Markdown") {
			t.Fatal("missing shared context or auto mode")
		}
		agentToolSessions.mu.Lock()
		var session *AgentToolSession
		for _, candidate := range agentToolSessions.sessions {
			if candidate.Mode == AgentToolAssistant && candidate.UserID == 43 {
				session = candidate
			}
		}
		agentToolSessions.mu.Unlock()
		if session == nil || !session.ExecMode.AutoWrite || session.ExecMode.ConfirmDangerous || session.ExecMode.DeferSOP {
			t.Fatal("Bot did not reuse full auto assistant")
		}
		session.commitFinal(AgentTerminalResult{Kind: AgentTerminalAssistant, Assistant: &assistantReply{Message: "查询完成 other-key"}})
		return "", context.Canceled
	}
	reply, err := h.generateBotReply(context.Background(), bot, domain.BotConversation{SessionID: "session", ChatID: "new-group", ChatType: "group"}, []domain.BotInteraction{{SenderID: "first-member", Content: "历史提问", Reply: "历史回复"}}, domain.BotInteraction{SenderID: "new-wecom-user", Content: "现在有什么进展"})
	if err != nil || reply != "查询完成 [redacted]" {
		t.Fatal(reply, err)
	}
	// No fallback to the Bot owner when the sender has a different identity.
	bot.InteractionEnabled = false
	s.SaveNotificationBot(bot, "")
	if _, err = h.botExecutionUser(bot, "new-wecom-user"); err == nil {
		t.Fatal("disabled Bot executed")
	}
}
func TestCardActionsAreScopedAndCannotApproveAnotherAttempt(t *testing.T) {
	s, h, bot := botFixture(t)
	run := domain.Run{ID: "card-run", Status: "waiting", WorkflowID: "wf", ExecutionContext: domain.TelemetryContext{ProjectID: "p"}, WorkflowSnapshot: domain.WorkflowDefinition{ID: "wf", ProjectID: "p", Nodes: []domain.WorkflowNode{{ID: "gate", Type: "human", Name: "人工确认"}}}, Nodes: map[string]*domain.NodeRun{"gate": {NodeID: "gate", Status: "waiting_approval", Attempt: 1, ReworkCount: 0}}}
	if err := s.PutRun(run); err != nil {
		t.Fatal(err)
	}
	delivery := domain.NotificationDelivery{ID: "d", BotID: bot.ID, ProjectID: "p", RunID: run.ID, NodeID: "gate", EventType: "waiting", Attempt: 1, TargetID: "group", TargetType: "group", Status: "sent", CreatedAt: time.Now().UTC()}
	if err := s.CreateNotificationDelivery(delivery); err != nil {
		t.Fatal(err)
	}
	item := domain.BotInteraction{DeliveryID: delivery.ID, Action: "approve", SenderID: "bound-member"}
	conv := domain.BotConversation{ChatID: "group", ChatType: "group"}
	foreign := conv
	foreign.ChatID = "other-group"
	if _, err := h.notifications.service.Interactions.NotificationCardTask(bot, item, foreign); err == nil {
		t.Fatal("foreign chat used card")
	}
	reply, err := h.handleBotCardAction(context.Background(), bot, item, conv)
	if err != nil || !strings.Contains(reply, "Ação concluída") {
		t.Fatal(reply, err)
	}
	updated, _ := s.RunState(run.ID)
	if updated.Nodes["gate"].Status != "succeeded" {
		t.Fatal("button did not approve")
	}
	reply, _ = h.handleBotCardAction(context.Background(), bot, item, conv)
	if !strings.Contains(reply, "expirou") {
		t.Fatal("duplicate card remained actionable")
	}
	updated.Status = "waiting"
	updated.Nodes["gate"].Status = "waiting_approval"
	updated.Nodes["gate"].Attempt = 2
	s.PutRun(updated)
	if actions := botinteraction.ActionsForDelivery(s, delivery); len(actions) != 0 {
		t.Fatal("old card applies to new attempt")
	}
	err = h.engine.Approve(run.ID, "gate", "old click", engine.NodeExpectation{NodeID: "gate", Status: "waiting_approval", Attempt: 1})
	if err == nil {
		t.Fatal("engine accepted stale expectation")
	}
	final, _ := s.RunState(run.ID)
	if final.Nodes["gate"].Status != "waiting_approval" || final.Nodes["gate"].Attempt != 2 {
		t.Fatal("old click mutated new attempt")
	}
}
func TestBotTerminalRetryCannotRepeatCachedWriteTool(t *testing.T) {
	session := &AgentToolSession{Mode: AgentToolAssistant, ExecMode: assistantExecModeForApproval(AssistantApprovalAuto)}
	session.SetTerminalOnly()
	gateway := &agentToolGateway{}
	_, err := gateway.callTool(context.Background(), session, []byte(`{"name":"agentflow__runs_retry","arguments":{"path":{"id":"r"},"body":{"nodeId":"n"}}}`))
	if err == nil {
		t.Fatal("terminal-only retry permitted cached write")
	}
}

func TestBotMissingSubmissionRetriesOnlyTerminalStep(t *testing.T) {
	_, h, bot := botFixture(t)
	calls := 0
	h.generate = func(ctx context.Context, prompt string, user domain.User) (string, error) {
		calls++
		if calls == 1 {
			return "请稍等，我正在查询项目的运行记录。", nil
		}
		if strings.Contains(prompt, "原始查询标记") || !strings.Contains(prompt, "Não consulte de novo") {
			t.Fatal("retry restarted task", prompt)
		}
		agentToolSessions.mu.Lock()
		var session *AgentToolSession
		for _, candidate := range agentToolSessions.sessions {
			if candidate.Mode == AgentToolAssistant && candidate.UserID == user.ID {
				session = candidate
			}
		}
		agentToolSessions.mu.Unlock()
		if session == nil || !session.terminalToolsOnly() {
			t.Fatal("retry permits repeated operations")
		}
		session.commitFinal(AgentTerminalResult{Kind: AgentTerminalAssistant, Assistant: &assistantReply{Message: "查询已提交"}})
		return "", context.Canceled
	}
	reply, err := h.generateBotReply(context.Background(), bot, domain.BotConversation{SessionID: "bot-session", ChatType: "single"}, nil, domain.BotInteraction{SenderID: "new-wecom-user", Content: "原始查询标记"})
	if err != nil || reply != "查询已提交" || calls != 2 {
		t.Fatal(reply, err, calls)
	}
}

func TestBotPlainAnswerNeedsNoSubmission(t *testing.T) {
	_, h, bot := botFixture(t)
	calls := 0
	h.generate = func(ctx context.Context, prompt string, user domain.User) (string, error) {
		calls++
		return "O requisito 8 está em implementação, na etapa de revisão.", nil
	}
	reply, err := h.generateBotReply(context.Background(), bot, domain.BotConversation{SessionID: "bot-session", ChatType: "single"}, nil, domain.BotInteraction{SenderID: "new-wecom-user", Content: "qual o status do requisito?"})
	if err != nil || reply != "O requisito 8 está em implementação, na etapa de revisão." || calls != 1 {
		t.Fatal(reply, err, calls)
	}
}

func bindBotTestUser(t *testing.T, s *repo.Store, bot domain.NotificationBot, sender string, userID int64) {
	t.Helper()
	now := time.Now().UTC()
	token, err := s.NewBotBindingChallenge(bot.BotID, sender, now)
	if err != nil {
		t.Fatal(err)
	}
	code, err := s.ClaimBotBindingChallenge(token, userID, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConfirmBotUserBinding(bot.BotID, sender, code, now); err != nil {
		t.Fatal(err)
	}
}
func TestBotUnboundAndMissingCredentialsNeverUseOwner(t *testing.T) {
	s, h, bot := botFixture(t)
	h.generate = func(context.Context, string, domain.User) (string, error) {
		t.Fatal("unbound actor reached model")
		return "", nil
	}
	reply, err := h.generateBotReply(context.Background(), bot, domain.BotConversation{}, nil, domain.BotInteraction{SenderID: "unknown", Content: "创建需求"})
	if err != nil || !strings.Contains(reply, "bot-bind.html#request=") {
		t.Fatal(reply, err)
	}
	if _, err := h.botExecutionUser(bot, "unknown"); err == nil {
		t.Fatal("unbound user used owner")
	}
	s.UpdateUserCredentials(43, repo.UserCredentialUpdate{CodeBuddyAPIKeyClear: true})
	reply, err = h.generateBotReply(context.Background(), bot, domain.BotConversation{}, nil, domain.BotInteraction{SenderID: "new-wecom-user", Content: "创建需求"})
	if err != nil || !strings.Contains(reply, "ainda não tem uma Key") {
		t.Fatal(reply, err)
	}
	project, _ := s.Project("p")
	project.Members = []domain.ProjectMember{}
	s.PutProject(project)
	if _, err = h.botExecutionUser(bot, "new-wecom-user"); err == nil {
		t.Fatal("revoked member inherited owner permission")
	}
}
func TestUnboundCardActorCannotMutateRun(t *testing.T) {
	s, h, bot := botFixture(t)
	run := domain.Run{ID: "identity-gate", Status: "waiting", Nodes: map[string]*domain.NodeRun{"gate": {NodeID: "gate", Status: "waiting_approval"}}}
	if err := s.PutRun(run); err != nil {
		t.Fatal(err)
	}
	reply, err := h.handleBotCardAction(context.Background(), bot, domain.BotInteraction{SenderID: "unknown", Action: "approve"}, domain.BotConversation{})
	if err != botinteraction.ErrCardActionDeferred || !strings.Contains(reply, "vincule sua conta da plataforma") {
		t.Fatal(reply, err)
	}
	after, _ := s.RunState(run.ID)
	if after.Nodes["gate"].Status != "waiting_approval" {
		t.Fatal("unbound click changed run")
	}
	project, _ := s.Project("p")
	project.Members = []domain.ProjectMember{{UserID: 43, Role: "viewer"}}
	s.PutProject(project)
	reply, err = h.handleBotCardAction(context.Background(), bot, domain.BotInteraction{SenderID: "new-wecom-user", Action: "retry"}, domain.BotConversation{})
	if err != botinteraction.ErrCardActionDeferred || !strings.Contains(reply, "não tem permissão para executar") {
		t.Fatal(reply, err)
	}
	if after, _ = s.RunState(run.ID); after.Nodes["gate"].Status != "waiting_approval" {
		t.Fatal("viewer click changed run")
	}
}

// A Bot on a Claude Code model replies without a CodeBuddy key: on the
// sender's Claude key or, without one, the platform subscription token.
func TestBotRepliesOnClaudeCodeWithoutCodeBuddyKey(t *testing.T) {
	s, h, bot := botFixture(t)
	s.UpdateUserCredentials(43, repo.UserCredentialUpdate{CodeBuddyAPIKeyClear: true})
	bot.Model = "claude-code:opus"
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	calls := 0
	h.generate = func(ctx context.Context, prompt string, user domain.User) (string, error) {
		calls++
		agentToolSessions.mu.Lock()
		var session *AgentToolSession
		for _, candidate := range agentToolSessions.sessions {
			if candidate.Mode == AgentToolAssistant && candidate.UserID == user.ID {
				session = candidate
			}
		}
		agentToolSessions.mu.Unlock()
		session.commitFinal(AgentTerminalResult{Kind: AgentTerminalAssistant, Assistant: &assistantReply{Message: "完成 sk-ant-oat01-bot"}})
		return "", context.Canceled
	}
	item := domain.BotInteraction{SenderID: "new-wecom-user", Content: "现在有什么进展"}
	reply, err := h.generateBotReply(context.Background(), bot, domain.BotConversation{ChatType: "single"}, nil, item)
	if err != nil || !strings.Contains(reply, "Key do Claude Code") || calls != 0 {
		t.Fatalf("no Claude credential: reply=%q err=%v calls=%d", reply, err, calls)
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-bot")
	reply, err = h.generateBotReply(context.Background(), bot, domain.BotConversation{ChatType: "single"}, nil, item)
	if err != nil || reply != "完成 [redacted]" || calls != 1 {
		t.Fatalf("platform token: reply=%q err=%v calls=%d", reply, err, calls)
	}
}

// telegramFixture answers getMe with the id before the token's colon, unless
// the token is listed in other.
func telegramFixture(t *testing.T, other map[string]string) cvBHTTPFixture {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, method, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/bot"), "/")
		if method != "getMe" {
			_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
			return
		}
		id, _, _ := strings.Cut(token, ":")
		if reply, ok := other[token]; ok {
			_, _ = w.Write([]byte(reply))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"id":` + id + `,"is_bot":true}}`))
	}))
	t.Cleanup(srv.Close)
	f := cvBNewHTTPFixture(t)
	svc := notification.New(f.s)
	svc.Telegram = &notification.TelegramProvider{HTTP: srv.Client(), BaseURL: srv.URL}
	f.h = NewWithNotifications(f.s, f.e, tracker.NewService(), "", "", svc)
	return f
}

func TestTelegramBotTakesItsIDFromTheToken(t *testing.T) {
	f := telegramFixture(t, map[string]string{
		"777:other-bot": `{"ok":true,"result":{"id":999,"is_bot":true}}`,
		"778:revoked":   `{"ok":false,"error_code":401,"description":"Unauthorized"}`,
	})
	body := map[string]any{"projectId": "platform", "name": "TG", "provider": "telegram", "botId": "ignored", "secret": "123456:AAE-token_x", "enabled": true, "interactionEnabled": true}
	rec := cvBRequest(t, f, http.MethodPost, "/api/notification-bots", body, true)
	var bot domain.NotificationBot
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &bot) != nil {
		t.Fatalf("create %d %s", rec.Code, rec.Body.String())
	}
	if bot.Provider != domain.BotProviderTelegram || bot.BotID != "telegram-123456" || !bot.InteractionEnabled {
		t.Fatalf("bot %+v", bot)
	}
	body["name"] = "TG copy"
	if rec = cvBRequest(t, f, http.MethodPost, "/api/notification-bots", body, true); rec.Code != http.StatusConflict || strings.Contains(rec.Body.String(), "企业微信") {
		t.Fatalf("second interactive config %d %s", rec.Code, rec.Body.String())
	}
	for _, token := range []string{"777:other-bot", "778:revoked"} {
		body["secret"] = token
		if rec = cvBRequest(t, f, http.MethodPost, "/api/notification-bots", body, true); rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), token) {
			t.Fatalf("unconfirmed token %s accepted: %d %s", token, rec.Code, rec.Body.String())
		}
	}
	bots, err := f.s.InteractiveNotificationBots()
	if err != nil || len(bots) != 1 || bots[0].ID != bot.ID {
		t.Fatalf("interactive bots %+v %v", bots, err)
	}
	body["secret"] = "123456:AAE-token_x"
	// Editing without a new token keeps the provider and the derived id.
	rec = cvBRequest(t, f, http.MethodPut, "/api/notification-bots/"+bot.ID, map[string]any{"projectId": "platform", "name": "TG 2", "provider": "", "botId": bot.BotID, "secret": "", "enabled": true}, true)
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &bot) != nil || bot.Provider != domain.BotProviderTelegram || bot.BotID != "telegram-123456" {
		t.Fatalf("edit %d %s", rec.Code, rec.Body.String())
	}
	body["secret"] = "not-a-token"
	if rec = cvBRequest(t, f, http.MethodPost, "/api/notification-bots", body, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad token accepted: %d", rec.Code)
	}
}

// The Bot editor saves Claude Code models; other CLIs stay refused.
func TestBotSavesClaudeCodeModel(t *testing.T) {
	f := cvBNewHTTPFixture(t)
	for model, want := range map[string]int{"claude-code:opus": http.StatusOK, "codex:gpt-5.5": http.StatusBadRequest, "claude-code:": http.StatusBadRequest} {
		rec := cvBRequest(t, f, http.MethodPost, "/api/notification-bots", map[string]any{
			"projectId": "platform", "name": "Bot", "botId": "claude-" + strings.ReplaceAll(model, ":", "-"), "secret": "secret", "enabled": true, "model": model,
		}, true)
		if rec.Code != want {
			t.Errorf("model %q = %d want %d: %s", model, rec.Code, want, rec.Body.String())
		}
	}
}

func TestTelegramBotPromptNamesTheAppAndAsksForPlainText(t *testing.T) {
	tg := domain.NotificationBot{Provider: domain.BotProviderTelegram}
	if botAppName(tg) != "Telegram" || !strings.Contains(botReplyFormat(tg), "sem Markdown") {
		t.Fatalf("telegram prompt parts %q %q", botAppName(tg), botReplyFormat(tg))
	}
}
