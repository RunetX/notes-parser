package store

// Методы личной переписки сайта (talks): диалоги (talks_peers), сообщения
// (talks_messages) и site-идентичность владельца сессии (колонки sessions.*).
// Маппинг «доставленное входящее ЛС → id сообщения в мессенджере» ведётся в
// общей message_targets (kind = TargetPMMessage, ref_id = talks_messages.id).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Направления сообщения talks.
const (
	TalkIn  = "in"  // входящее (от собеседника)
	TalkOut = "out" // исходящее (отправленное с нашей стороны)
)

// TalkPeer — собеседник в talks (один диалог владельца сессии).
type TalkPeer struct {
	ID          int64
	Messenger   string
	OwnerUserID int64
	PassportID  string
	ProfileID   string
	Nick        string
	AvatarURL   string
	CursorMsgID string    // последнее втянутое сообщение сайта
	LastEventAt time.Time // zero — событий ещё не было
	CreatedAt   time.Time
}

// TalkMessage — одно сообщение диалога talks.
type TalkMessage struct {
	ID        int64
	PeerID    int64
	SiteMsgID string // "" — исходящее до подтверждения (в БД NULL)
	Direction string // TalkIn | TalkOut
	Text      string // "" при store_text=false
	MediaURL  string
	SentAt    time.Time // время по сайту; zero — неизвестно
	CreatedAt time.Time
}

// UpsertTalkPeer заводит собеседника или обновляет непустые ник/аватар/анкету
// (latest-wins по непустому значению, как в архивной дедупликации типажей).
// Курсор и last_event_at здесь не трогаются — их двигает SetPeerCursor.
func (s *Store) UpsertTalkPeer(ctx context.Context, p TalkPeer) (int64, error) {
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO talks_peers
			(messenger, owner_user_id, passport_id, profile_id, nick, avatar_url,
			 cursor_msg_id, last_event_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, '', ?, ?)
		ON CONFLICT(messenger, owner_user_id, passport_id) DO UPDATE SET
			profile_id = CASE WHEN excluded.profile_id != '' THEN excluded.profile_id ELSE profile_id END,
			nick       = CASE WHEN excluded.nick       != '' THEN excluded.nick       ELSE nick       END,
			avatar_url = CASE WHEN excluded.avatar_url != '' THEN excluded.avatar_url ELSE avatar_url END`,
		p.Messenger, p.OwnerUserID, p.PassportID, p.ProfileID, p.Nick, p.AvatarURL,
		nullTime(p.LastEventAt), fmtTime(time.Now())); err != nil {
		return 0, fmt.Errorf("upsert talk peer %s/%d/%s: %w", p.Messenger, p.OwnerUserID, p.PassportID, err)
	}
	var id int64
	if err := s.db.QueryRowContext(ctx, `
		SELECT id FROM talks_peers WHERE messenger = ? AND owner_user_id = ? AND passport_id = ?`,
		p.Messenger, p.OwnerUserID, p.PassportID).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// TalkPeers возвращает диалоги владельца сессии, свежие сверху (для /talks и
// обхода поллером).
func (s *Store) TalkPeers(ctx context.Context, messenger string, ownerUserID int64) ([]TalkPeer, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, messenger, owner_user_id, passport_id, profile_id, nick, avatar_url,
		       cursor_msg_id, last_event_at, created_at
		FROM talks_peers WHERE messenger = ? AND owner_user_id = ?
		ORDER BY last_event_at DESC NULLS LAST, id`, messenger, ownerUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var peers []TalkPeer
	for rows.Next() {
		p, err := scanTalkPeer(rows)
		if err != nil {
			return nil, err
		}
		peers = append(peers, p)
	}
	return peers, rows.Err()
}

// TalkPeerByID возвращает собеседника по внутреннему id. ErrNotFound — нет.
func (s *Store) TalkPeerByID(ctx context.Context, id int64) (TalkPeer, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, messenger, owner_user_id, passport_id, profile_id, nick, avatar_url,
		       cursor_msg_id, last_event_at, created_at
		FROM talks_peers WHERE id = ?`, id)
	if err != nil {
		return TalkPeer{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return TalkPeer{}, err
		}
		return TalkPeer{}, fmt.Errorf("собеседник talks %d: %w", id, ErrNotFound)
	}
	return scanTalkPeer(rows)
}

// SetPeerCursor двигает курсор диалога и время последнего события.
func (s *Store) SetPeerCursor(ctx context.Context, peerID int64, cursorMsgID string, lastEventAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE talks_peers SET cursor_msg_id = ?, last_event_at = ? WHERE id = ?`,
		cursorMsgID, nullTime(lastEventAt), peerID)
	return err
}

// InsertTalkMessage сохраняет сообщение, дедуплицируя входящие по
// (peer_id, site_msg_id). fresh=false — сообщение уже было (частичный уникальный
// индекс; исходящие с NULL site_msg_id никогда не конфликтуют). При конфликте
// возвращается id уже существующей строки — доставка идемпотентна по
// message_targets и может использовать его, не завися от свежести вставки.
func (s *Store) InsertTalkMessage(ctx context.Context, m TalkMessage) (id int64, fresh bool, err error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO talks_messages
			(peer_id, site_msg_id, direction, text, media_url, sent_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		m.PeerID, nullStr(m.SiteMsgID), m.Direction, m.Text, m.MediaURL,
		nullTime(m.SentAt), fmtTime(time.Now()))
	if err != nil {
		return 0, false, fmt.Errorf("insert talk message peer=%d: %w", m.PeerID, err)
	}
	if affected, _ := res.RowsAffected(); affected > 0 {
		id, _ = res.LastInsertId()
		return id, true, nil
	}
	// Конфликт по (peer_id, site_msg_id) — вернём id существующей строки.
	if m.SiteMsgID == "" {
		return 0, false, nil
	}
	err = s.db.QueryRowContext(ctx, `
		SELECT id FROM talks_messages WHERE peer_id = ? AND site_msg_id = ?`,
		m.PeerID, m.SiteMsgID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id, false, err
}

// HasUndeliveredIncoming — есть ли у диалога входящее, не уехавшее в мессенджер:
// строка talks_messages без пары в message_targets. Строка пишется ДО попытки
// доставки, поэтому сбой мессенджера виден в БД и переживает рестарт демона —
// по этому признаку поллер переспрашивает историю, не дожидаясь нового
// сообщения на сайте. since ограничивает окно: сообщение, уехавшее за первую
// страницу истории сайта, дотянуть уже нечем (только `talks -backfill`), и без
// границы оно заставляло бы перезапрашивать диалог вечно.
func (s *Store) HasUndeliveredIncoming(ctx context.Context, messenger string, peerID int64, since time.Time) (bool, error) {
	var found int
	if err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM talks_messages m
			WHERE m.peer_id = ? AND m.direction = ? AND m.created_at >= ?
			  AND NOT EXISTS (
				SELECT 1 FROM message_targets t
				WHERE t.messenger = ? AND t.kind = ? AND t.ref_id = CAST(m.id AS TEXT)))`,
		peerID, TalkIn, fmtTime(since), messenger, TargetPMMessage).Scan(&found); err != nil {
		return false, fmt.Errorf("недоставленные ЛС peer=%d: %w", peerID, err)
	}
	return found == 1, nil
}

// PeerByDeliveredPM находит собеседника по id доставленного в мессенджер ЛС
// (для маршрутизации ответа реплаем: reply-to → message_targets → peer).
// ErrNotFound — это сообщение не связано с диалогом talks.
func (s *Store) PeerByDeliveredPM(ctx context.Context, messenger, deliveredMsgID string) (TalkPeer, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.messenger, p.owner_user_id, p.passport_id, p.profile_id, p.nick,
		       p.avatar_url, p.cursor_msg_id, p.last_event_at, p.created_at
		FROM message_targets t
		JOIN talks_messages m ON m.id = CAST(t.ref_id AS INTEGER)
		JOIN talks_peers    p ON p.id = m.peer_id
		WHERE t.messenger = ? AND t.kind = ? AND t.message_id = ?`,
		messenger, TargetPMMessage, deliveredMsgID)
	if err != nil {
		return TalkPeer{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return TalkPeer{}, err
		}
		return TalkPeer{}, fmt.Errorf("диалог по доставленному ЛС %s/%s: %w", messenger, deliveredMsgID, ErrNotFound)
	}
	return scanTalkPeer(rows)
}

// SessionOwners возвращает id пользователей с валидной сессией сайта в
// мессенджере (кого обходит поллер talks).
func (s *Store) SessionOwners(ctx context.Context, messenger string) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT user_id FROM sessions WHERE messenger = ? AND valid = 1 ORDER BY user_id`, messenger)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SessionIdentity возвращает site-идентичность владельца сессии (id анкеты,
// паспорт, ник). Пустые строки — ещё не заполнено. ErrNotFound — нет сессии.
func (s *Store) SessionIdentity(ctx context.Context, messenger string, userID int64) (profileID, passportID, nick string, err error) {
	err = s.db.QueryRowContext(ctx, `
		SELECT site_profile_id, site_passport_id, site_nick
		FROM sessions WHERE messenger = ? AND user_id = ?`, messenger, userID).
		Scan(&profileID, &passportID, &nick)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", fmt.Errorf("сессия пользователя %s/%d: %w", messenger, userID, ErrNotFound)
	}
	return profileID, passportID, nick, err
}

// SetSessionIdentity проставляет site-идентичность владельца сессии (снимается
// с авторизованной страницы при /login и лениво при первом успешном запросе).
func (s *Store) SetSessionIdentity(ctx context.Context, messenger string, userID int64, profileID, passportID, nick string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET site_profile_id = ?, site_passport_id = ?, site_nick = ?
		WHERE messenger = ? AND user_id = ?`,
		profileID, passportID, nick, messenger, userID)
	return err
}

// PurgeTalksOlderThan удаляет сообщения talks старше cutoff (ретеншен
// приватности). Собеседники остаются — они лёгкие метаданные.
func (s *Store) PurgeTalksOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM talks_messages WHERE created_at < ?`, fmtTime(cutoff))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func scanTalkPeer(rows *sql.Rows) (TalkPeer, error) {
	var p TalkPeer
	var lastEvent sql.NullString
	var createdAt string
	if err := rows.Scan(&p.ID, &p.Messenger, &p.OwnerUserID, &p.PassportID, &p.ProfileID,
		&p.Nick, &p.AvatarURL, &p.CursorMsgID, &lastEvent, &createdAt); err != nil {
		return p, err
	}
	var err error
	if lastEvent.Valid {
		if p.LastEventAt, err = parseTime(lastEvent.String); err != nil {
			return p, err
		}
	}
	if p.CreatedAt, err = parseTime(createdAt); err != nil {
		return p, err
	}
	return p, nil
}

func scanTalkMessage(rows *sql.Rows) (TalkMessage, error) {
	var m TalkMessage
	var siteMsg, sentAt sql.NullString
	var createdAt string
	if err := rows.Scan(&m.ID, &m.PeerID, &siteMsg, &m.Direction, &m.Text,
		&m.MediaURL, &sentAt, &createdAt); err != nil {
		return m, err
	}
	m.SiteMsgID = siteMsg.String
	var err error
	if sentAt.Valid {
		if m.SentAt, err = parseTime(sentAt.String); err != nil {
			return m, err
		}
	}
	if m.CreatedAt, err = parseTime(createdAt); err != nil {
		return m, err
	}
	return m, nil
}
