package botinteraction

import (
	"agentflow/internal/domain"
	"errors"
	"fmt"
	"log"

	"github.com/go-sql-driver/mysql"
)

// A legacy utf8mb3 table cannot store emoji. Preserve a clear terminal reply
// rather than reconnecting with an orphaned running request and no explanation.
func (s *Service) saveReply(item domain.BotInteraction, reply, detail string) (string, string, error) {
	err := s.Store.SaveBotReply(item, reply, detail)
	if err == nil {
		return reply, detail, nil
	}
	var sqlErr *mysql.MySQLError
	if errors.As(err, &sqlErr) {
		log.Printf("wecom reply persistence failed id=%s mysql_code=%d", item.ID, sqlErr.Number)
		if sqlErr.Number == 1366 || sqlErr.Number == 3988 {
			reply = "A resposta da IA foi gerada, mas o banco de dados não salvou alguns caracteres. Peça ao administrador para converter a tabela de mensagens do bot para utf8mb4 e envie o pedido de novo."
			detail = fmt.Sprintf("Falha ao salvar a mensagem: MySQL %d. Converta o charset de bot_interactions para utf8mb4", sqlErr.Number)
			err = s.Store.SaveBotReply(item, reply, detail)
			return reply, detail, err
		}
	} else {
		log.Printf("wecom reply persistence failed id=%s error_type=%T", item.ID, err)
	}
	return reply, detail, err
}
