package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Rememorio/gofer/internal/channel"
	"github.com/Rememorio/gofer/internal/domain"
)

type channelState struct{ database *SQL }

func (state *channelState) Bind(ctx context.Context, binding channel.Binding) (channel.Binding, error) {
	if err := binding.Validate(); err != nil || binding.Status != channel.BindingConnected {
		return channel.Binding{}, channel.ErrInvalid
	}
	tx, err := state.database.db.BeginTx(ctx, nil)
	if err != nil {
		return channel.Binding{}, err
	}
	defer func() { _ = tx.Rollback() }()
	existing, err := state.scanBinding(tx.QueryRowContext(ctx, state.database.bind(channelBindingByIdentity), binding.Provider, binding.WorkspaceID, binding.ExternalUserID))
	if err != nil && !errors.Is(err, channel.ErrNotFound) {
		return channel.Binding{}, err
	}
	if err == nil {
		return state.updateChannelBinding(ctx, tx, existing, binding)
	}
	return state.insertChannelBinding(ctx, tx, binding)
}

func (state *channelState) updateChannelBinding(ctx context.Context, tx *sql.Tx, existing, incoming channel.Binding) (channel.Binding, error) {
	if existing.Status == channel.BindingConnected && existing.UserID != incoming.UserID {
		return channel.Binding{}, channel.ErrConflict
	}
	expectedOwner, expectedStatus := existing.UserID, existing.Status
	if existing.UserID != incoming.UserID {
		if _, err := tx.ExecContext(ctx, state.database.bind(`DELETE FROM gofer_channel_conversations WHERE binding_id=?`), existing.ID); err != nil {
			return channel.Binding{}, err
		}
	}
	existing.UserID, existing.Status, existing.UpdatedAt = incoming.UserID, channel.BindingConnected, incoming.UpdatedAt.UTC()
	existing.WorkspaceName, existing.ExternalUserName = incoming.WorkspaceName, incoming.ExternalUserName
	result, err := tx.ExecContext(ctx, state.database.bind(`UPDATE gofer_channel_bindings SET user_id=?,workspace_name=?,external_user_name=?,status=?,updated_at=? WHERE id=? AND user_id=? AND status=?`),
		existing.UserID, existing.WorkspaceName, existing.ExternalUserName, existing.Status, formatChannelTime(existing.UpdatedAt), existing.ID, expectedOwner, expectedStatus)
	if err != nil {
		return existing, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return existing, err
	}
	if affected != 1 {
		return existing, channel.ErrConflict
	}
	return existing, tx.Commit()
}

func (state *channelState) insertChannelBinding(ctx context.Context, tx *sql.Tx, binding channel.Binding) (channel.Binding, error) {
	result, err := tx.ExecContext(ctx, state.database.bind(`INSERT INTO gofer_channel_bindings(id,user_id,provider,workspace_id,workspace_name,external_user_id,external_user_name,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(provider,workspace_id,external_user_id) DO NOTHING`),
		binding.ID, binding.UserID, binding.Provider, binding.WorkspaceID, binding.WorkspaceName, binding.ExternalUserID, binding.ExternalUserName, binding.Status, formatChannelTime(binding.CreatedAt), formatChannelTime(binding.UpdatedAt))
	if err != nil {
		return channel.Binding{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return channel.Binding{}, err
	}
	if affected != 1 {
		_ = tx.Rollback()
		return state.bindAfterInsertConflict(ctx, binding)
	}
	return binding, tx.Commit()
}

func (state *channelState) Bindings(ctx context.Context, userID string) ([]channel.Binding, error) {
	rows, err := state.database.db.QueryContext(ctx, state.database.bind(`SELECT id,user_id,provider,workspace_id,workspace_name,external_user_id,external_user_name,status,created_at,updated_at FROM gofer_channel_bindings WHERE user_id=? ORDER BY updated_at DESC,id DESC`), userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	bindings := make([]channel.Binding, 0)
	for rows.Next() {
		binding, scanErr := state.scanBinding(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		bindings = append(bindings, binding)
	}
	return bindings, rows.Err()
}

func (state *channelState) Revoke(ctx context.Context, identifier, userID string, at time.Time) error {
	result, err := state.database.db.ExecContext(ctx, state.database.bind(`UPDATE gofer_channel_bindings SET status=?,updated_at=? WHERE id=? AND user_id=?`), channel.BindingRevoked, formatChannelTime(at.UTC()), identifier, userID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return channel.ErrNotFound
	}
	return nil
}

func (state *channelState) Resolve(ctx context.Context, provider, workspaceID, externalUserID string) (channel.Identity, error) {
	var identity channel.Identity
	err := state.database.db.QueryRowContext(ctx, state.database.bind(`SELECT id,user_id FROM gofer_channel_bindings WHERE provider=? AND workspace_id=? AND external_user_id=? AND status=?`), provider, workspaceID, externalUserID, channel.BindingConnected).Scan(&identity.BindingID, &identity.UserID)
	if errors.Is(err, sql.ErrNoRows) {
		return channel.Identity{}, channel.ErrNotFound
	}
	return identity, err
}

func (state *channelState) Conversation(ctx context.Context, bindingID, chatID, topicID string) (channel.Conversation, error) {
	row := state.database.db.QueryRowContext(ctx, state.database.bind(`SELECT c.binding_id,c.provider,c.chat_id,c.topic_id,c.thread_id,c.created_at,c.updated_at FROM gofer_channel_conversations c JOIN gofer_channel_bindings b ON b.id=c.binding_id WHERE c.binding_id=? AND c.chat_id=? AND c.topic_id=? AND b.status=?`), bindingID, chatID, topicID, channel.BindingConnected)
	return scanChannelConversation(row)
}

func (state *channelState) MapConversation(ctx context.Context, conversation channel.Conversation) (channel.Conversation, bool, error) {
	if err := conversation.Validate(); err != nil {
		return channel.Conversation{}, false, err
	}
	tx, err := state.database.db.BeginTx(ctx, nil)
	if err != nil {
		return channel.Conversation{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var provider string
	if err = tx.QueryRowContext(ctx, state.database.bind(`SELECT provider FROM gofer_channel_bindings WHERE id=? AND status=?`), conversation.BindingID, channel.BindingConnected).Scan(&provider); errors.Is(err, sql.ErrNoRows) {
		return channel.Conversation{}, false, channel.ErrNotFound
	} else if err != nil {
		return channel.Conversation{}, false, err
	}
	if provider != conversation.Provider {
		return channel.Conversation{}, false, channel.ErrInvalid
	}
	result, err := tx.ExecContext(ctx, state.database.bind(`INSERT INTO gofer_channel_conversations(binding_id,provider,chat_id,topic_id,thread_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(binding_id,chat_id,topic_id) DO NOTHING`),
		conversation.BindingID, conversation.Provider, conversation.ChatID, conversation.TopicID, conversation.ThreadID, formatChannelTime(conversation.CreatedAt), formatChannelTime(conversation.UpdatedAt))
	if err != nil {
		return channel.Conversation{}, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return channel.Conversation{}, false, err
	}
	if affected == 1 {
		return conversation, true, tx.Commit()
	}
	existing, err := scanChannelConversation(tx.QueryRowContext(ctx, state.database.bind(`SELECT binding_id,provider,chat_id,topic_id,thread_id,created_at,updated_at FROM gofer_channel_conversations WHERE binding_id=? AND chat_id=? AND topic_id=?`), conversation.BindingID, conversation.ChatID, conversation.TopicID))
	if err != nil {
		return channel.Conversation{}, false, err
	}
	return existing, false, tx.Commit()
}

func (state *channelState) DeleteThread(ctx context.Context, threadID domain.ThreadID) error {
	_, err := state.database.db.ExecContext(ctx, state.database.bind(`DELETE FROM gofer_channel_conversations WHERE thread_id=?`), threadID)
	return err
}

func (state *channelState) Begin(ctx context.Context, key string, now time.Time, ttl time.Duration) (bool, error) {
	if key == "" || now.IsZero() || ttl <= 0 {
		return false, channel.ErrInvalid
	}
	tx, err := state.database.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, state.database.bind(`DELETE FROM gofer_channel_deliveries WHERE expires_at<=?`), now.UTC().UnixNano()); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, state.database.bind(`INSERT INTO gofer_channel_deliveries(delivery_key,expires_at,complete) VALUES(?,?,?) ON CONFLICT(delivery_key) DO NOTHING`), key, now.Add(ttl).UTC().UnixNano(), false)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, tx.Commit()
}

func (state *channelState) Complete(ctx context.Context, key string, success bool) error {
	query := `DELETE FROM gofer_channel_deliveries WHERE delivery_key=?`
	arguments := []any{key}
	if success {
		query = `UPDATE gofer_channel_deliveries SET complete=? WHERE delivery_key=?`
		arguments = []any{true, key}
	}
	result, err := state.database.db.ExecContext(ctx, state.database.bind(query), arguments...)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return channel.ErrInvalid
	}
	return nil
}

const channelBindingByIdentity = `SELECT id,user_id,provider,workspace_id,workspace_name,external_user_id,external_user_name,status,created_at,updated_at FROM gofer_channel_bindings WHERE provider=? AND workspace_id=? AND external_user_id=?`

func (state *channelState) bindAfterInsertConflict(ctx context.Context, binding channel.Binding) (channel.Binding, error) {
	existing, err := state.scanBinding(state.database.db.QueryRowContext(ctx, state.database.bind(channelBindingByIdentity), binding.Provider, binding.WorkspaceID, binding.ExternalUserID))
	if err != nil {
		return channel.Binding{}, err
	}
	if existing.Status == channel.BindingConnected && existing.UserID != binding.UserID {
		return channel.Binding{}, channel.ErrConflict
	}
	return state.Bind(ctx, binding)
}

func formatChannelTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
}

func (state *channelState) scanBinding(row interface{ Scan(...any) error }) (channel.Binding, error) {
	var binding channel.Binding
	var createdAt, updatedAt string
	err := row.Scan(&binding.ID, &binding.UserID, &binding.Provider, &binding.WorkspaceID, &binding.WorkspaceName, &binding.ExternalUserID, &binding.ExternalUserName, &binding.Status, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return channel.Binding{}, channel.ErrNotFound
	}
	if err != nil {
		return channel.Binding{}, err
	}
	binding.CreatedAt, binding.UpdatedAt = parseTime(createdAt), parseTime(updatedAt)
	if err = binding.Validate(); err != nil {
		return channel.Binding{}, fmt.Errorf("decode channel binding: %w", err)
	}
	return binding, nil
}

func scanChannelConversation(row interface{ Scan(...any) error }) (channel.Conversation, error) {
	var conversation channel.Conversation
	var createdAt, updatedAt string
	err := row.Scan(&conversation.BindingID, &conversation.Provider, &conversation.ChatID, &conversation.TopicID, &conversation.ThreadID, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return channel.Conversation{}, channel.ErrNotFound
	}
	if err != nil {
		return channel.Conversation{}, err
	}
	conversation.CreatedAt, conversation.UpdatedAt = parseTime(createdAt), parseTime(updatedAt)
	if err = conversation.Validate(); err != nil {
		return channel.Conversation{}, fmt.Errorf("decode channel conversation: %w", err)
	}
	return conversation, nil
}
