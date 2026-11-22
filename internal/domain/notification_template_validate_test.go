package domain

import (
	"strings"
	"testing"
)

func TestValidateNotificationTemplate(t *testing.T) {
	for _, tc := range []struct {
		name, template, want string
	}{
		{"empty", "", ""},
		{"plain text", "流程已结束", ""},
		{"all supported variables", "{{project}} {{ workflow }} {{node}} {{status}} {{url}} {{issue.title}} {{issue.id}} {{input}} {{result}} {{error}} {{task.name}} {{ai.summary}} {{ai.title}} {{ai.body}}", ""},
		{"unknown variable", "{{status}} {{secret}}", "O modelo do aviso tem uma variável não suportada: secret"},
		{"unfinished placeholder", "{{unfinished", "Formato de variável inválido no modelo do aviso"},
		{"stray closing braces", "done }}", "Formato de variável inválido no modelo do aviso"},
		{"invalid variable name", "{{ 1abc }}", "Formato de variável inválido no modelo do aviso"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertNotificationError(t, validateNotificationTemplate(tc.template), tc.want)
		})
	}
}

func TestValidateNotificationEvents(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []string
		scope  string
		want   string
	}{
		{"single node event", []string{"succeeded"}, "", ""},
		{"all node events", []string{"succeeded", "failed", "waiting"}, "node", ""},
		{"run without waiting", []string{"succeeded", "failed"}, "run", ""},
		{"empty", nil, "", "Escolha quando o aviso dispara"},
		{"more than three", []string{"succeeded", "failed", "waiting", "succeeded"}, "", "Escolha quando o aviso dispara"},
		{"duplicate", []string{"failed", "failed"}, "", "Condição de disparo do aviso não suportada"},
		{"unknown", []string{"started"}, "", "Condição de disparo do aviso não suportada"},
		{"run waiting", []string{"succeeded", "waiting"}, "run", "Condição de disparo do aviso não suportada"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertNotificationError(t, validateNotificationEvents(tc.events, tc.scope), tc.want)
		})
	}
}

func TestValidateNotificationBindingCheckOrder(t *testing.T) {
	for _, tc := range []struct {
		name    string
		binding NotificationBinding
		want    string
	}{
		{"valid", NotificationBinding{Events: []string{"succeeded"}, Template: "{{status}}"}, ""},
		{"scope before template", NotificationBinding{Scope: "bad", Template: "{{secret}}"}, "Escopo do aviso inválido"},
		{"template length before variables", NotificationBinding{Template: strings.Repeat("{{secret}}", 1201)}, "O modelo do aviso não pode passar de 12000 bytes"},
		{"instruction length before variables", NotificationBinding{AIInstruction: strings.Repeat("x", 4001), Template: "{{secret}}"}, "As instruções para a IA escrever o aviso não podem passar de 4000 bytes"},
		{"template before events", NotificationBinding{Template: "{{secret}}"}, "O modelo do aviso tem uma variável não suportada: secret"},
		{"events last", NotificationBinding{Template: "{{status}}", Scope: "run", Events: []string{"waiting"}}, "Condição de disparo do aviso não suportada"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertNotificationError(t, ValidateNotificationBinding(tc.binding), tc.want)
		})
	}
}

func assertNotificationError(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}
