package repo

import (
	"agentflow/internal/domain"
	"fmt"
)

func (r *CatalogRepo) validateWorkflowNotifications(w domain.WorkflowDefinition) error {
	runBots := map[string]bool{}
	if len(w.Notifications) > 1 {
		return fmt.Errorf("Cada SOP aceita só um bot de aviso")
	}
	if len(w.Notifications) > 0 {
		if w.ProjectID == "" || w.Shared || w.Generic {
			return fmt.Errorf("O aviso de bot só pode ficar num SOP deste projeto. Copie o modelo compartilhado primeiro")
		}
		if err := r.notifications.ValidateNotificationBindings(w.ProjectID, w.Notifications, false); err != nil {
			return err
		}
		for _, b := range w.Notifications {
			if b.Scope != "run" || len(b.Events) != 1 || b.Events[0] != "succeeded" {
				return fmt.Errorf("Aviso de SOP só aceita a conclusão do SOP inteiro")
			}
			runBots[b.BotID] = true
		}
	}
	for _, node := range w.Nodes {
		if len(node.Notifications) == 0 {
			continue
		}
		if node.Type != "agent" {
			return fmt.Errorf("Só nós de agente aceitam aviso")
		}
		if w.ProjectID == "" || w.Shared || w.Generic {
			return fmt.Errorf("O aviso de bot só pode ficar num SOP deste projeto. Copie o modelo compartilhado primeiro")
		}
		if err := r.notifications.ValidateNotificationBindings(w.ProjectID, node.Notifications, false); err != nil {
			return err
		}
		for _, binding := range node.Notifications {
			if binding.Scope == "run" {
				if runBots[binding.BotID] {
					return fmt.Errorf("O aviso do SOP inteiro só pode ser configurado uma vez por bot")
				}
				runBots[binding.BotID] = true
			}
		}
	}
	return nil
}
