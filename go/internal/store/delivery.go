package store

// Выбор мессенджера доставки личных сообщений сайта (talks). Одно и то же
// входящее ЛС нельзя принести в два мессенджера: сайт помечает сообщение
// прочитанным в тот момент, когда поллер забирает историю, и второй сессии
// достанется пустота (`getMessagesHistory` = mark-read, разбор от 11.08.2026).
// Поэтому у сайт-аккаунта ровно один получатель, а выбор — исключающий: колонки
// sessions.talks_delivery / talks_asked_at (миграция v8).
//
// Сессии связывает в один аккаунт site_passport_id: человек входит в каждом
// мессенджере отдельно, и общего у двух строк sessions больше ничего нет.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Значения sessions.talks_delivery.
const (
	DeliveryUnset = ""    // человек не выбирал — решаем за него (PickDelivery)
	DeliveryOn    = "on"  // носить ЛС в этот мессенджер
	DeliveryOff   = "off" // сюда не носить
)

// TalksOwner — владелец сессии сайта глазами доставки ЛС: одна строка sessions.
type TalksOwner struct {
	Messenger  string
	UserID     int64
	PassportID string // '' — идентичность не снята: связать с другим мессенджером нечем
	Delivery   string // DeliveryUnset | DeliveryOn | DeliveryOff
	Asked      bool   // спрашивали ли уже, куда носить (спрашиваем один раз)
	// UpdatedAt — когда человек вошёл. Свежесть входа решает спор двух сессий,
	// пока выбора нет: пользуются обычно тем мессенджером, где вошли последним.
	UpdatedAt time.Time
}

// AccountKey — ключ сайт-аккаунта для группировки сессий. Без паспорта сессия
// считается отдельным аккаунтом: слить по пустому значению — значит объявить
// одним человеком всех, у кого идентичность ещё не снята.
func (o TalksOwner) AccountKey() string {
	if o.PassportID != "" {
		return "p:" + o.PassportID
	}
	return "s:" + o.Messenger + ":" + strconv.FormatInt(o.UserID, 10)
}

// TalksOwners возвращает все валидные сессии сайта во всех мессенджерах —
// вход поллера talks: решение «куда носить» межмессенджерное по природе, по
// одному мессенджеру за раз его не принять.
func (s *Store) TalksOwners(ctx context.Context) ([]TalksOwner, error) {
	return s.talksOwners(ctx, `
		SELECT messenger, user_id, site_passport_id, talks_delivery, talks_asked_at, updated_at
		FROM sessions WHERE valid = 1 ORDER BY messenger, user_id`)
}

// TalksAccount возвращает сессии сайт-аккаунта этого пользователя: его
// собственную (даже истёкшую — настройку показываем и после протухшей сессии) и
// валидные сессии того же паспорта в остальных мессенджерах. ErrNotFound —
// человек ни разу не входил, настраивать нечего.
func (s *Store) TalksAccount(ctx context.Context, messenger string, userID int64) ([]TalksOwner, error) {
	own, err := s.talksOwners(ctx, `
		SELECT messenger, user_id, site_passport_id, talks_delivery, talks_asked_at, updated_at
		FROM sessions WHERE messenger = ? AND user_id = ?`, messenger, userID)
	if err != nil {
		return nil, err
	}
	if len(own) == 0 {
		return nil, fmt.Errorf("сессия пользователя %s/%d: %w", messenger, userID, ErrNotFound)
	}
	if own[0].PassportID == "" {
		return own, nil // связать не с чем: аккаунт из одной сессии
	}
	others, err := s.talksOwners(ctx, `
		SELECT messenger, user_id, site_passport_id, talks_delivery, talks_asked_at, updated_at
		FROM sessions
		WHERE site_passport_id = ? AND valid = 1 AND NOT (messenger = ? AND user_id = ?)
		ORDER BY messenger, user_id`, own[0].PassportID, messenger, userID)
	if err != nil {
		return nil, err
	}
	return append(own, others...), nil
}

// querier — общее у *sql.DB и *sql.Tx: сессии, которым сейчас выключат
// доставку, читаются внутри транзакции, остальные — прямым запросом.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func (s *Store) talksOwners(ctx context.Context, query string, args ...any) ([]TalksOwner, error) {
	return talksOwners(ctx, s.db, query, args...)
}

func talksOwners(ctx context.Context, q querier, query string, args ...any) ([]TalksOwner, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("сессии talks: %w", err)
	}
	defer rows.Close()
	var owners []TalksOwner
	for rows.Next() {
		var o TalksOwner
		var asked sql.NullString
		var updated string
		if err := rows.Scan(&o.Messenger, &o.UserID, &o.PassportID, &o.Delivery, &asked, &updated); err != nil {
			return nil, err
		}
		o.Asked = asked.Valid
		if o.UpdatedAt, err = parseTime(updated); err != nil {
			return nil, err
		}
		owners = append(owners, o)
	}
	return owners, rows.Err()
}

// GroupByAccount группирует сессии по сайт-аккаунту. Группы и сессии внутри них
// идут в порядке входного списка: по этим группам рассылают вопрос человеку, и
// случайный порядок map'а сделал бы рассылку невоспроизводимой.
func GroupByAccount(owners []TalksOwner) [][]TalksOwner {
	var groups [][]TalksOwner
	at := make(map[string]int, len(owners))
	for _, o := range owners {
		k := o.AccountKey()
		if i, ok := at[k]; ok {
			groups[i] = append(groups[i], o)
			continue
		}
		at[k] = len(groups)
		groups = append(groups, []TalksOwner{o})
	}
	return groups
}

// PickDelivery — в какой мессенджер носить ЛС сайт-аккаунта. ok == false —
// никуда: человек отказался всюду. Правило: явный выбор сильнее всего, а пока
// его нет — самый свежий вход (при равенстве побеждает меньшее имя мессенджера,
// чтобы решение не плавало от такта к такту).
func PickDelivery(group []TalksOwner) (TalksOwner, bool) {
	var best TalksOwner
	found := false
	for _, o := range group {
		if o.Delivery == DeliveryOff {
			continue
		}
		if !found || better(o, best) {
			best, found = o, true
		}
	}
	return best, found
}

// better — кто из двух сессий получает ЛС: явный выбор бьёт молчание, среди
// равных — свежесть входа, при полном равенстве решает имя мессенджера.
func better(a, b TalksOwner) bool {
	if (a.Delivery == DeliveryOn) != (b.Delivery == DeliveryOn) {
		return a.Delivery == DeliveryOn
	}
	if !a.UpdatedAt.Equal(b.UpdatedAt) {
		return a.UpdatedAt.After(b.UpdatedAt)
	}
	return less(a, b)
}

// less — устойчивый порядок сессий при одинаковой свежести входа.
func less(a, b TalksOwner) bool {
	if a.Messenger != b.Messenger {
		return a.Messenger < b.Messenger
	}
	return a.UserID < b.UserID
}

// SetTalksDelivery записывает выбор человека. DeliveryOn выключает доставку
// остальным сессиям того же сайт-аккаунта — в этом весь смысл настройки, второй
// мессенджер иначе продолжит гасить непрочитанное на сайте. Возвращает
// выключенные сессии (о них человеку и сообщают). Невалидные сессии тоже
// выключаются: истёкшая сессия однажды оживёт повторным /login, и выбор должен
// пережить это. ErrNotFound — сессии нет.
func (s *Store) SetTalksDelivery(ctx context.Context, messenger string, userID int64, choice string, now time.Time) ([]TalksOwner, error) {
	if choice != DeliveryOn && choice != DeliveryOff {
		return nil, fmt.Errorf("неизвестный выбор доставки %q", choice)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var passport string
	err = tx.QueryRowContext(ctx, `
		SELECT site_passport_id FROM sessions WHERE messenger = ? AND user_id = ?`,
		messenger, userID).Scan(&passport)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("сессия пользователя %s/%d: %w", messenger, userID, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE sessions SET talks_delivery = ?, talks_asked_at = ?
		WHERE messenger = ? AND user_id = ?`,
		choice, fmtTime(now), messenger, userID); err != nil {
		return nil, fmt.Errorf("выбор доставки %s/%d: %w", messenger, userID, err)
	}
	var off []TalksOwner
	if choice == DeliveryOn && passport != "" {
		off, err = talksOwners(ctx, tx, `
			SELECT messenger, user_id, site_passport_id, talks_delivery, talks_asked_at, updated_at
			FROM sessions
			WHERE site_passport_id = ? AND talks_delivery != ? AND NOT (messenger = ? AND user_id = ?)
			ORDER BY messenger, user_id`, passport, DeliveryOff, messenger, userID)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE sessions SET talks_delivery = ?, talks_asked_at = ?
			WHERE site_passport_id = ? AND NOT (messenger = ? AND user_id = ?)`,
			DeliveryOff, fmtTime(now), passport, messenger, userID); err != nil {
			return nil, fmt.Errorf("снятие доставки у сессий паспорта %s: %w", passport, err)
		}
	}
	return off, tx.Commit()
}

// MarkTalksAsked отмечает, что человека уже спрашивали, куда носить ЛС.
// Отметка живёт в БД, а не в памяти: иначе каждый рестарт демона переспрашивал
// бы заново.
func (s *Store) MarkTalksAsked(ctx context.Context, messenger string, userID int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET talks_asked_at = ? WHERE messenger = ? AND user_id = ?`,
		fmtTime(now), messenger, userID)
	if err != nil {
		return fmt.Errorf("отметка вопроса о доставке %s/%d: %w", messenger, userID, err)
	}
	return nil
}
