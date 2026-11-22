-- Optional manual additive DDL; startup AutoMigrate also adds this column.
-- Telegram conversations record the sender name shown in the target list.
SET @bot_chat_name_ddl = IF(
 (SELECT COUNT(*) FROM information_schema.columns
  WHERE table_schema=DATABASE() AND table_name='bot_conversations' AND column_name='chat_name')=0,
 'ALTER TABLE bot_conversations ADD COLUMN chat_name VARCHAR(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT ''''',
 'SELECT 1');
PREPARE bot_chat_name_stmt FROM @bot_chat_name_ddl;
EXECUTE bot_chat_name_stmt;
DEALLOCATE PREPARE bot_chat_name_stmt;
