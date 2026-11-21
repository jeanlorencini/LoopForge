package domain

import "testing"

func TestNotificationTemplateDefaults(t *testing.T) {
	cases := []struct {
		name       string
		binding    NotificationBinding
		sourceType string
		want       string
	}{
		{"custom template wins", NotificationBinding{ContentMode: "ai", Scope: "run", Template: "自定义 {{status}}"}, "schedule_job", "自定义 {{status}}"},
		{"blank template falls back", NotificationBinding{Template: "  \n"}, "", "### {{status}}\nProjeto: {{project}}\nFluxo: {{workflow}}\nNó: {{node}}\nDetalhes: {{url}}"},
		{"ai mode beats run scope", NotificationBinding{ContentMode: "ai", Scope: "run"}, "schedule_job", "### {{ai.title}}\n\n{{ai.body}}\n\nStatus no envio: {{status}}\n\n{{url}}"},
		{"run scope beats source type", NotificationBinding{Scope: "run"}, "assistant_task", "### SOP do requisito “{{issue.title}}” concluído\n\n{{result}}\n\n{{url}}"},
		{"schedule job", NotificationBinding{}, "schedule_job", "### {{task.name}} · {{status}}\n\n{{url}}"},
		{"assistant task", NotificationBinding{}, "assistant_task", "### {{task.name}} · {{status}}\n\n{{url}}"},
		{"node default", NotificationBinding{}, "run", "### {{status}}\nProjeto: {{project}}\nFluxo: {{workflow}}\nNó: {{node}}\nDetalhes: {{url}}"},
	}
	for _, tc := range cases {
		if got := notificationTemplate(tc.binding, tc.sourceType); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestRenderNotificationUsesScheduleDefaultTemplate(t *testing.T) {
	binding := NotificationBinding{Events: []string{"succeeded"}}
	content, source, err := RenderNotification(binding, NotificationContext{SourceType: "schedule_job", Values: map[string]string{"task.name": "日报", "status": "已完成"}})
	if err != nil || source != "template" || content != "### 日报 · 已完成\n\n"+NotificationURLMarker {
		t.Fatalf("%q %s %v", content, source, err)
	}
}
