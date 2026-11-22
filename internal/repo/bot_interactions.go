package repo

import (
	"agentflow/internal/domain"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type BotInteractionsRepo struct{ *deps }

func BotConversationID(botID, kind, chatID string) string {
	return hexSHA256(botID + "\x00" + kind + "\x00" + chatID)
}
func BotMessageID(botID, msgID string) string {
	return hexSHA256(botID + "\x00" + msgID)
}
func newBotSessionID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(err)
	}
	return "bot-" + hex.EncodeToString(raw[:])
}
func (r *BotInteractionsRepo) ClaimBotConnection(botID, owner string, now time.Time) (bool, error) {
	row := domain.BotConnection{BotID: botID, State: "connecting", LeaseUntil: now.Add(-time.Second)}
	if err := r.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
		return false, err
	}
	result := r.DB.Model(&row).Where("bot_id = ? AND (owner = ? OR lease_until < ?)", botID, owner, now).Updates(map[string]any{"owner": owner, "lease_until": now.Add(45 * time.Second), "updated_at": now})
	return result.RowsAffected == 1, result.Error
}
func (r *BotInteractionsRepo) RenewBotConnection(botID, owner, state, detail string, now time.Time) (bool, error) {
	updates := map[string]any{"lease_until": now.Add(45 * time.Second), "updated_at": now}
	if state != "" {
		updates["state"] = state
		updates["last_error"] = detail
	}
	result := r.DB.Model(&domain.BotConnection{}).Where("bot_id = ? AND owner = ? AND lease_until > ?", botID, owner, now).Updates(updates)
	return result.RowsAffected == 1, result.Error
}
func (r *BotInteractionsRepo) ReleaseBotConnection(botID, owner string) error {
	return r.DB.Model(&domain.BotConnection{}).Where("bot_id = ? AND owner = ?", botID, owner).Updates(map[string]any{"owner": "", "lease_until": time.Now().UTC().Add(-time.Second), "state": "offline"}).Error
}
func (r *BotInteractionsRepo) BotConnectionState(botID string) (domain.BotConnection, error) {
	var out domain.BotConnection
	err := r.DB.First(&out, "bot_id = ?", botID).Error
	if out.LeaseUntil.Before(time.Now().UTC()) {
		out.State = "offline"
	}
	return out, err
}
func (r *BotInteractionsRepo) EnqueueBotInteraction(item domain.BotInteraction, kind, chat string) (bool, error) {
	inserted := false
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		conversation := domain.BotConversation{ID: BotConversationID(item.BotID, kind, chat), BotID: item.BotID, ChatID: chat, ChatType: kind, SessionID: newBotSessionID(), UpdatedAt: time.Now().UTC()}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&conversation).Error; err != nil {
			return err
		}
		now := time.Now().UTC()
		item.ConversationID = conversation.ID
		item.Status = "queued"
		// A transport that knows when the message was sent (Telegram) sets
		// CreatedAt, so process can refuse a backlog kept during an outage.
		if item.CreatedAt.IsZero() {
			item.CreatedAt = now
		}
		item.UpdatedAt = now
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&item)
		inserted = result.RowsAffected == 1
		if result.Error != nil {
			return result.Error
		}
		if inserted {
			return tx.Model(&conversation).Update("updated_at", now).Error
		}
		return nil
	})
	return inserted, err
}
func (r *BotInteractionsRepo) ClaimBotInteraction(botID, leaseBotID, owner string) (*domain.BotInteraction, error) {
	var item domain.BotInteraction
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		var lease domain.BotConnection
		if err := r.locking(tx).Where("bot_id = ? AND owner = ? AND lease_until > ?", leaseBotID, owner, time.Now().UTC()).First(&lease).Error; err != nil {
			return err
		}
		if err := r.locking(tx).Where("bot_id = ? AND status IN ?", botID, []string{"queued", "ready"}).Order("created_at, id").First(&item).Error; err != nil {
			return err
		}
		if item.Status == "ready" {
			return nil
		}
		var conv domain.BotConversation
		if err := tx.First(&conv, "id = ?", item.ConversationID).Error; err != nil {
			return err
		}
		item.SessionID = conv.SessionID
		item.Status = "running"
		return tx.Model(&item).Updates(map[string]any{"session_id": item.SessionID, "status": "running", "updated_at": time.Now().UTC()}).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

// A model turn interrupted by process loss must never be replayed automatically:
// its platform tools may already have committed. Its reply can still be retried.
func (r *BotInteractionsRepo) RecoverBotInteractions(botID string) error {
	return r.DB.Model(&domain.BotInteraction{}).Where("bot_id = ? AND status = ?", botID, "running").Updates(map[string]any{"status": "ready", "reply": "A rodada anterior foi interrompida, e o resultado pode já ter sido salvo. Consulte o status antes de decidir se continua.", "last_error": "Execução interrompida", "updated_at": time.Now().UTC()}).Error
}
func (r *BotInteractionsRepo) SaveBotReply(item domain.BotInteraction, reply, detail string) error {
	return r.DB.Model(&domain.BotInteraction{}).Where("id = ? AND status = ?", item.ID, "running").Updates(map[string]any{"status": "ready", "reply": reply, "last_error": detail, "updated_at": time.Now().UTC()}).Error
}
func (r *BotInteractionsRepo) MarkBotReplySent(id string) error {
	return r.DB.Model(&domain.BotInteraction{}).Where("id = ? AND status = ?", id, "ready").Updates(map[string]any{"status": "replied", "updated_at": time.Now().UTC()}).Error
}
func (r *BotInteractionsRepo) NewBotConversation(item domain.BotInteraction) error {
	return r.DB.Transaction(func(tx *gorm.DB) error {
		session := newBotSessionID()
		if err := tx.Model(&domain.BotConversation{}).Where("id = ?", item.ConversationID).Updates(map[string]any{"session_id": session, "updated_at": time.Now().UTC()}).Error; err != nil {
			return err
		}
		return tx.Model(&domain.BotInteraction{}).Where("id = ?", item.ID).Updates(map[string]any{"session_id": session, "action": "new"}).Error
	})
}
func (r *BotInteractionsRepo) BotConversation(id string) (domain.BotConversation, error) {
	var out domain.BotConversation
	err := r.DB.First(&out, "id = ?", id).Error
	return out, err
}
func (r *BotInteractionsRepo) BotHistory(sessionID string) ([]domain.BotInteraction, error) {
	var rows []domain.BotInteraction
	err := r.DB.Where("session_id = ? AND status = ? AND action = ?", sessionID, "replied", "").Order("created_at desc, id desc").Limit(20).Find(&rows).Error
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows, err
}
func (r *BotInteractionsRepo) BotInteractionRecords(botID string) ([]domain.BotInteraction, error) {
	var rows []domain.BotInteraction
	err := r.DB.Where("bot_id = ?", botID).Order("created_at desc, id desc").Limit(50).Find(&rows).Error
	return rows, err
}
func (r *BotInteractionsRepo) BotConversations(botID string) ([]domain.BotConversation, error) {
	var rows []domain.BotConversation
	err := r.DB.Where("bot_id = ?", botID).Order("updated_at desc").Limit(50).Find(&rows).Error
	return rows, err
}

func (r *BotInteractionsRepo) NameBotConversation(id, name string) error {
	return r.DB.Model(&domain.BotConversation{}).Where("id = ? AND chat_name <> ?", id, name).Update("chat_name", name).Error
}

// BoundBotChats lists the private chats of senders bound to platform accounts,
// newest first. The binding filter runs before the limit, so chats of unbound
// strangers cannot push bound chats off the list.
func (r *BotInteractionsRepo) BoundBotChats(botID, externalBotID string) ([]domain.BotConversation, error) {
	var rows []domain.BotConversation
	err := r.DB.Where("bot_id = ? AND chat_type = ?", botID, "single").
		Where("chat_id IN (?)", r.DB.Model(&domain.BotUserBinding{}).Select("sender_id").Where("bot_id = ? AND user_id > 0", externalBotID)).
		Order("updated_at desc").Limit(50).Find(&rows).Error
	return rows, err
}

// BotCardByTaskID finds the notification card a Telegram button belongs to.
func (r *BotInteractionsRepo) BotCardByTaskID(botID, taskID string) (domain.BotInteraction, error) {
	var item domain.BotInteraction
	err := r.DB.First(&item, "bot_id = ? AND action = ? AND card_task_id = ?", botID, "notification", taskID).Error
	return item, err
}

func (r *BotInteractionsRepo) BotInteraction(id string) (domain.BotInteraction, error) {
	var item domain.BotInteraction
	err := r.DB.First(&item, "id = ?", id).Error
	return item, err
}

func (r *BotInteractionsRepo) ExpireBotCard(id string) error {
	return r.DB.Model(&domain.BotInteraction{}).Where("id = ? AND action = ? AND status <> ?", id, "notification", "replied").Updates(map[string]any{"status": "replied", "last_error": "Este nó não espera mais por esta ação", "updated_at": time.Now().UTC()}).Error
}

func (r *BotInteractionsRepo) FailBotReply(id, detail string) error {
	return r.DB.Model(&domain.BotInteraction{}).Where("id = ? AND status = ?", id, "ready").Updates(map[string]any{"status": "reply_failed", "last_error": detail, "updated_at": time.Now().UTC()}).Error
}
func (r *BotInteractionsRepo) RequeueBotCard(id string) error {
	return r.DB.Model(&domain.BotInteraction{}).Where("id = ? AND action = ? AND status = ?", id, "notification", "reply_failed").Updates(map[string]any{"status": "ready", "last_error": "", "updated_at": time.Now().UTC()}).Error
}

// Serializes registering the same external Bot across project configs. The
// receiver later claims this same connection row, avoiding duplicate sockets.
func (r *BotInteractionsRepo) lockBotDefinition(tx *gorm.DB, botID string) error {
	row := domain.BotConnection{BotID: botID, LeaseUntil: time.Now().UTC().Add(-time.Second), State: "offline"}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
		return err
	}
	return r.locking(tx).Where("bot_id = ?", botID).First(&row).Error
}

// Store the exact opaque task ID before wire send so callbacks cannot route by
// a guessed delivery prefix or a stale card from an earlier send attempt.
func (r *BotInteractionsRepo) SetBotCardTaskID(id, taskID string) error {
	result := r.DB.Model(&domain.BotInteraction{}).Where("id = ? AND action = ? AND status = ?", id, "notification", "ready").Update("card_task_id", taskID)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return gorm.ErrRecordNotFound
	}
	return nil
}
