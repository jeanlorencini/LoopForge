package httpapi

import (
	"agentflow/internal/domain"
	"errors"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"net/url"
	"strings"
	"time"
)

type botIdentityHandlers struct {
	*deps
	publicURL string
}

func (h *botIdentityHandlers) currentUser(c *gin.Context) (domain.User, bool) {
	// Binding is an interactive login operation, never a Bearer-token operation.
	if _, ok := bearerToken(c.Request); ok {
		c.JSON(401, gin.H{"error": "Entre pelo site da plataforma para confirmar sua identidade"})
		return domain.User{}, false
	}
	current, ok := h.requestUser(c.Request, false)
	if !ok || current == nil || current.IsGuest || current.ID <= 0 {
		c.JSON(401, gin.H{"error": "Entre na plataforma primeiro"})
		return domain.User{}, false
	}
	user, ok := h.repo.User(current.ID)
	if !ok {
		c.JSON(401, gin.H{"error": "A conta da plataforma não existe"})
		return user, false
	}
	return user, true
}
func (h *botIdentityHandlers) binding(c *gin.Context) {
	user, ok := h.currentUser(c)
	if !ok {
		return
	}
	c.Header("Cache-Control", "no-store")
	var input struct {
		Token string `json:"token"`
	}
	if c.ShouldBindJSON(&input) != nil || len(input.Token) != 64 {
		c.JSON(400, gin.H{"error": "Link de vínculo inválido"})
		return
	}
	status, err := h.repo.BotBindingChallengeStatus(input.Token, user.ID, time.Now().UTC())
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if strings.HasSuffix(c.Request.URL.Path, "/claim") && status != "confirmed" {
		if !botBindingSameOrigin(c) {
			c.JSON(403, gin.H{"error": "Use a página de vínculo da plataforma"})
			return
		}
		code, err := h.repo.ClaimBotBindingChallenge(input.Token, user.ID, time.Now().UTC())
		if err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"status": "claimed", "loginName": user.LoginName, "command": "/bind " + code})
		return
	}
	c.JSON(200, gin.H{"status": status, "loginName": user.LoginName})
}
func botBindingSameOrigin(c *gin.Context) bool {
	origin, err := url.Parse(c.GetHeader("Origin"))
	return err == nil && (origin.Scheme == "http" || origin.Scheme == "https") && origin.Host == c.Request.Host
}
func (h *botIdentityHandlers) list(c *gin.Context) {
	user, ok := h.currentUser(c)
	if !ok {
		return
	}
	bindings, err := h.repo.BotUserBindings(user.ID)
	if err != nil {
		respond(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, bindings)
}
func (h *botIdentityHandlers) remove(c *gin.Context) {
	user, ok := h.currentUser(c)
	if !ok {
		return
	}
	if !botBindingSameOrigin(c) {
		c.JSON(403, gin.H{"error": "Desvincule pelas configurações pessoais"})
		return
	}
	if err := h.repo.RevokeBotUserBinding(c.Param("id"), user.ID, time.Now().UTC()); err != nil {
		c.JSON(404, gin.H{"error": "Vínculo não encontrado"})
		return
	}
	c.Status(204)
}
func botIdentityCommand(content string) string {
	text := strings.TrimSpace(content)
	if strings.HasPrefix(text, "@") {
		if _, rest, ok := strings.Cut(text, " "); ok {
			text = strings.TrimSpace(rest)
		}
	}
	return text
}

// Only platform login nominates an account. The trusted callback from the
// original WeCom sender must submit the browser-only code before it takes effect.
func (h *botIdentityHandlers) message(bot domain.NotificationBot, item domain.BotInteraction) (bool, string, error) {
	text := botIdentityCommand(item.Content)
	if text == "/unbind" || text == "解除绑定" {
		binding, err := h.repo.BotUserBinding(bot.BotID, item.SenderID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return true, "Esta conta do chat ainda não está vinculada a uma conta da plataforma. Envie /bind para vincular.", nil
		}
		if err != nil {
			return true, "", err
		}
		if err := h.repo.RevokeBotUserBinding(binding.ID, binding.UserID, time.Now().UTC()); err != nil {
			return true, "", err
		}
		return true, "O vínculo desta conta do chat com a plataforma foi removido, e o código antigo deixou de valer. Para vincular de novo, envie /bind.", nil
	}

	if strings.HasPrefix(text, "/bind ") {
		binding, err := h.repo.ConfirmBotUserBinding(bot.BotID, item.SenderID, strings.TrimSpace(strings.TrimPrefix(text, "/bind ")), time.Now().UTC())
		if err != nil {
			return true, err.Error(), nil
		}
		user, ok := h.repo.User(binding.UserID)
		if !ok {
			return true, "A conta da plataforma não existe. Desvincule nas configurações pessoais e vincule de novo.", nil
		}
		return true, "Conta da plataforma \"" + user.LoginName + "\" vinculada. As próximas ações usam sua identidade, suas credenciais pessoais e suas permissões de projeto. Envie de novo o pedido anterior.", nil
	}
	if text == "绑定账号" || text == "/bind" {
		if binding, err := h.repo.BotUserBinding(bot.BotID, item.SenderID); err == nil {
			if user, ok := h.repo.User(binding.UserID); ok {
				return true, "Esta conta já está vinculada a \"" + user.LoginName + "\". Envie /unbind ou desvincule nas configurações pessoais da plataforma.", nil
			}
		}
	}
	_, err := h.repo.BotUserBinding(bot.BotID, item.SenderID)
	if err == nil {
		return false, "", nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return true, "", err
	}
	base, err := url.Parse(strings.TrimRight(h.publicURL, "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return true, "Vincule sua conta da plataforma primeiro. O endereço externo da plataforma não está configurado, fale com o administrador.", nil
	}
	token, err := h.repo.NewBotBindingChallenge(bot.BotID, item.SenderID, time.Now().UTC())
	if err != nil {
		return true, "", err
	}
	link := strings.TrimRight(h.publicURL, "/") + "/bot-bind.html#request=" + url.QueryEscape(token)
	steps := " (vale por 15 minutos). Depois de entrar e confirmar, mande para este bot o código /bind mostrado na página. Se outra pessoa do grupo clicar no link, o vínculo não é concluído."
	// Telegram replies go out as plain text, so a Markdown link would show raw.
	if bot.Provider == domain.BotProviderTelegram {
		return true, "Vincule sua conta da plataforma primeiro: " + link + steps, nil
	}
	return true, "Primeiro [vincule sua conta da plataforma](" + link + ")" + steps, nil
}
