package httpapi

import (
	"agentflow/internal/domain"
	"agentflow/internal/notification"
	"agentflow/internal/runtime"
	"context"
	"os"
	"path/filepath"
	"strings"
)

func (h *assistantHandlers) generateSOPReport(ctx context.Context, run domain.Run, model, prompt string) (domain.AINotificationContent, error) {
	return h.generateReport(ctx, run, model, prompt, "你是整体交付报告编写者。只能根据提供的资料总结结果。仅允许调用 submit_notification_report，必须通过该工具提交报告，不能用普通文本代替。")
}
func (h *assistantHandlers) generateReport(ctx context.Context, run domain.Run, model, prompt, system string) (domain.AINotificationContent, error) {
	user, ok := h.repo.User(run.ExecutionContext.ResponsibleUserID)
	if !ok {
		return domain.AINotificationContent{}, notification.ErrReportCredentials
	}
	claude, err := platformModelCredential(ctx, user, model)
	if err != nil {
		return domain.AINotificationContent{}, err
	}

	modelCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := ensureAssistantSessionDirs(user.ID); err != nil {
		return domain.AINotificationContent{}, notification.ErrReportCLI
	}
	root, cleanup, err := claudeCallRoot(user.ID, claude)
	if err != nil {
		return domain.AINotificationContent{}, notification.ErrReportCLI
	}
	defer cleanup()
	session, files, err := startAgentToolSession(ctx, user, run.ExecutionContext.ProjectID, AgentToolReport, assistantExecMode{}, "", nil, cancel, filepath.Join(assistantSessionRoot(user.ID), "mcp-reports"))
	if err != nil {
		return domain.AINotificationContent{}, notification.ErrReportCLI
	}
	defer session.Close()
	defer files.Cleanup()
	session.RunID = run.ID
	session.SetTerminalOnly()
	generate := h.generate
	if generate == nil {
		generate = func(callCtx context.Context, input string, u domain.User) (string, error) {
			// Claude Code has no report-only mode; the terminal-only session limits
			// it to submit_notification_report.
			raw, _, err := runAssistantCLI(callCtx, assistantCLIRequest{Prompt: input, Model: model, UserID: u.ID, ReportOnly: claude == nil, Native: true, MCPConfig: files.MCPConfig, Settings: files.Settings, SystemPrompt: system, ClaudeCode: claude, SessionRoot: root}, u, nil)
			return raw, err
		}
	}
	for attempt := 0; attempt <= nativeTerminalMaxRetries; attempt++ {
		input := prompt
		if attempt > 0 {
			input = "上一轮没有有效提交报告。请修正参数并调用 submit_notification_report；普通文本不会被接收。\n" + prompt
		}
		_, cliErr := generate(modelCtx, input, user)
		if result, ok := session.takeFinal(); ok && result.Report != nil {
			report := *result.Report
			report.Title = redactAssistantSecrets(report.Title, user)
			report.Summary = redactAssistantSecrets(report.Summary, user)
			report.Body = redactAssistantSecrets(report.Body, user)
			return report, nil
		}
		if ctx.Err() != nil {
			return domain.AINotificationContent{}, ctx.Err()
		}
		if cliErr != nil && !nativeShouldRetryTerminal(ctx, cliErr) {
			return domain.AINotificationContent{}, notification.ErrReportCLI
		}
	}
	return domain.AINotificationContent{}, notification.ErrReportFormat
}

// platformModelCredential checks that user can run model in a platform-side
// call (Bot replies, SOP reports): CodeBuddy on the user's key and catalog,
// Claude Code on the user's key or the platform subscription token, as
// clarification does. It returns the Claude credential for runAssistantCLI.
func platformModelCredential(ctx context.Context, user domain.User, model string) (*runtime.ProviderCredential, error) {
	switch engine, name := assistantModelEngine(model); engine {
	case assistantEngineClaude:
		cred := clarifyClaudeCredential(user)
		if cred.APIKey == "" && cred.OAuthToken == "" {
			return nil, notification.ErrReportCredentials
		}
		if name == "" {
			return nil, notification.ErrReportModel
		}
		return &cred, nil
	case assistantEngineCodeBuddy:
		if strings.TrimSpace(user.CodeBuddyAPIKey) == "" {
			return nil, notification.ErrReportCredentials
		}
		if assistantAllowedModel(model, loadAssistantModels(ctx, user.CodeBuddyInternetEnvironment)) != model {
			return nil, notification.ErrReportModel
		}
		return nil, nil
	}
	return nil, notification.ErrReportModel
}

// claudeCallRoot gives a Claude Code call its own session root: Claude Code
// writes its MCP config and system prompt there, so concurrent calls of one
// user must not share the user's root. CodeBuddy calls keep that root ("").
func claudeCallRoot(userID int64, claude *runtime.ProviderCredential) (string, func(), error) {
	if claude == nil {
		return "", func() {}, nil
	}
	root, err := os.MkdirTemp(assistantSessionRoot(userID), "claude-call-")
	if err != nil {
		return "", nil, err
	}
	return root, func() { _ = os.RemoveAll(root) }, nil
}
