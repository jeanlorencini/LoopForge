package notification

import (
	"agentflow/internal/domain"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"
)

type Provider interface {
	Sessions(context.Context, string, string) ([]domain.BotChat, error)
	Send(context.Context, string, string, string, string) error
}
type ProviderError struct {
	Code    int
	Message string
	Source  string
}

func (e *ProviderError) Error() string {
	if e.Source != "" {
		return fmt.Sprintf("Erro do %s: %s", e.Source, e.Message)
	}
	return fmt.Sprintf("Erro do WeCom %d: %s", e.Code, e.Message)
}
func failure(code int) *ProviderError {
	message := "A requisição falhou. Confira a autorização do bot ou tente mais tarde"
	switch code {
	case 850003:
		message = "A permissão de mensagens expirou. Quem criou o bot precisa autorizar de novo"
	case 853004:
		message = "A credencial ou o token de acesso do bot expirou. Valide de novo"
	}
	return &ProviderError{Code: code, Message: message}
}

// CLIProvider keeps service discovery and message encoding inside the official CLI.
// Only the signed bootstrap is implemented here to support platform-side credential setup without a TTY.
type CLIProvider struct {
	HTTP    *http.Client
	Binary  string
	authURL string
}

func NewCLIProvider() *CLIProvider {
	return &CLIProvider{HTTP: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, Binary: "wecom-cli", authURL: "https://qyapi.weixin.qq.com/cgi-bin/aibot/cli/get_cli_config"}
}
func (p *CLIProvider) token(ctx context.Context, botID, secret string) (string, error) {
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	nonce := "cli_" + hex.EncodeToString(random)
	timestamp := time.Now().Unix()
	digest := sha256.Sum256([]byte(secret + botID + strconv.FormatInt(timestamp, 10) + nonce))
	body, _ := json.Marshal(map[string]any{"bot_id": botID, "time": timestamp, "nonce": nonce, "signature": hex.EncodeToString(digest[:]), "bind_source": 1})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.authURL, bytes.NewReader(body))
	if err != nil {
		return "", errors.New("Falha ao montar o pedido de autenticação do WeCom")
	}
	req.Header.Set("Content-Type", "application/json")
	rsp, err := p.HTTP.Do(req)
	if err != nil {
		return "", errors.New("Falha de rede na autenticação do WeCom")
	}
	defer rsp.Body.Close()
	if rsp.StatusCode != http.StatusOK {
		return "", errors.New("O serviço de autenticação do WeCom está indisponível")
	}
	var result struct {
		Code  int    `json:"errcode"`
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(rsp.Body, 1<<20)).Decode(&result); err != nil {
		return "", errors.New("Resposta da autenticação do WeCom em formato inesperado")
	}
	if result.Code != 0 {
		return "", failure(result.Code)
	}
	if result.Token == "" {
		return "", errors.New("A autenticação do WeCom não devolveu token")
	}
	return result.Token, nil
}
func (p *CLIProvider) call(ctx context.Context, botID, secret string, args ...string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "agentflow-wecom-")
	if err != nil {
		return nil, errors.New("A pasta temporária de avisos está indisponível")
	}
	defer os.RemoveAll(dir)
	for attempt := 0; attempt < 2; attempt++ {
		token, err := p.token(ctx, botID, secret)
		if err != nil {
			return nil, err
		}
		cmd := exec.CommandContext(ctx, p.Binary, args...)
		cmd.Env = wecomCLIEnv(token, dir)
		var out boundedBuffer
		cmd.Stdout = &out
		cmd.Stderr = io.Discard
		err = cmd.Run()
		if ctx.Err() != nil {
			return nil, errors.New("O envio do aviso ao WeCom demorou demais")
		}
		if errors.Is(err, exec.ErrNotFound) {
			return nil, errors.New("O serviço de avisos não tem o wecom-cli instalado. Fale com o administrador")
		}
		code, parseErr := wecomResultCode(out.Bytes())
		if parseErr != nil {
			return nil, parseErr
		}
		if code == 853004 && attempt == 0 {
			continue
		}
		if code != 0 {
			return nil, failure(code)
		}
		if err != nil {
			return nil, errors.New("Falha ao chamar o CLI do WeCom")
		}
		return append(json.RawMessage(nil), out.Bytes()...), nil
	}
	return nil, failure(853004)
}

// wecomCLIEnv builds the isolated wecom-cli environment: only a whitelist of
// host variables is inherited, plus the per-call access token and config dir.
func wecomCLIEnv(token, dir string) []string {
	var env []string
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return append(env, "WECOM_CLI_ACCESS_TOKEN="+token, "WECOM_CLI_CONFIG_DIR="+dir)
}

// wecomResultCode extracts the WeCom error code from wecom-cli output; a
// nested error.code takes precedence over the top-level errcode.
func wecomResultCode(raw []byte) (int, error) {
	var result struct {
		Code  int `json:"errcode"`
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return 0, errors.New("Resposta do CLI do WeCom em formato inesperado")
	}
	if result.Error != nil {
		return result.Error.Code, nil
	}
	return result.Code, nil
}

type boundedBuffer struct{ bytes.Buffer }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 2<<20 {
		return 0, errors.New("notification response too large")
	}
	return b.Buffer.Write(p)
}
func (p *CLIProvider) Sessions(ctx context.Context, botID, secret string) ([]domain.BotChat, error) {
	raw, err := p.call(ctx, botID, secret, "message", "aibot", "sessions", "list")
	if err != nil {
		return nil, err
	}
	var rsp struct {
		Sessions []struct {
			ID   string `json:"chat_id"`
			Name string `json:"chat_name"`
			Type string `json:"chat_type"`
			Last string `json:"last_msg_time"`
		} `json:"sessions"`
	}
	if err = json.Unmarshal(raw, &rsp); err != nil {
		return nil, errors.New("Lista de chats do WeCom em formato inesperado")
	}
	chats := make([]domain.BotChat, 0, len(rsp.Sessions))
	for _, s := range rsp.Sessions {
		if s.ID != "" && (s.Type == "single" || s.Type == "group") {
			chats = append(chats, domain.BotChat{ID: s.ID, Name: s.Name, Type: s.Type, LastMessageTime: s.Last})
		}
	}
	return chats, nil
}
func (p *CLIProvider) Send(ctx context.Context, botID, secret, target, content string) error {
	if len([]byte(content)) == 0 || len([]byte(content)) > 20480 {
		return errors.New("O texto do aviso precisa ter de 1 a 20480 bytes")
	}
	body, _ := json.Marshal(map[string]any{"chat_id": target, "msg_type": "markdown", "markdown": map[string]string{"content": content}})
	raw, err := p.call(ctx, botID, secret, "message", "aibot", "send", "--json", string(body))
	if err != nil {
		return err
	}
	var result struct {
		Success bool `json:"success"`
	}
	if json.Unmarshal(raw, &result) != nil || !result.Success {
		return errors.New("O WeCom não confirmou o envio")
	}
	return nil
}
