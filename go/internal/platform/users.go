package platform

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// querier — общий знаменатель пула и транзакции. Нужен, чтобы одну и ту же
// операцию (завести тень автора) можно было выполнить и отдельно, и внутри
// транзакции приёма, не заводя двух её копий.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const userColumns = `id, nick, avatar_sha, ngs_avatar_url, kind, role,
	hide_all, anonymized_at, banned_until, created_at, last_seen_at`

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Nick, &u.AvatarSHA, &u.NGSAvatarURL, &u.Kind, &u.Role,
		&u.HideAll, &u.AnonymizedAt, &u.BannedUntil, &u.CreatedAt, &u.LastSeenAt)
	return u, err
}

// UserByID возвращает строку пользователя.
func (p *Platform) UserByID(ctx context.Context, id int64) (User, error) {
	u, err := scanUser(p.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, fmt.Errorf("пользователь %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return User{}, fmt.Errorf("чтение пользователя %d: %w", id, err)
	}
	return u, nil
}

// EnsureShadow заводит тень для автора, увиденного через зеркало, и обновляет
// ник. Возвращает id (0 — автора нет: зеркальный аноним или комментатор без
// ссылки на анкету, для такого тень заводить не из чего).
//
// Тень — это не заготовка «на будущее», а несущая конструкция: у любой
// зеркальной публикации есть author_id, поэтому вход человека кодом в анкете НГС
// не переносит ни строки — он лишь меняет kind у уже существующего ряда, и весь
// след прошлых лет мгновенно становится его.
func (p *Platform) EnsureShadow(ctx context.Context, a MirroredAuthor) (int64, error) {
	return ensureShadow(ctx, p.pool, a)
}

func ensureShadow(ctx context.Context, q querier, a MirroredAuthor) (int64, error) {
	if a.ID == 0 {
		return 0, nil
	}
	if !IsNGS(a.ID) {
		return 0, fmt.Errorf("тень заводится только под id анкеты НГС, получен %d", a.ID)
	}
	// Ник обновляем latest-wins, но ТОЛЬКО у тени и только у необезличенного:
	//   * у вошедшего участника ник его собственный, и зеркало не вправе его
	//     переписывать (иначе смена ника на НГС отменяла бы выбор человека у нас);
	//   * у обезличенного возврат ника из зеркала отменял бы исполненное
	//     требование субъекта — то есть чинил бы нарушение закона каждым обходом.
	_, err := q.Exec(ctx, `
		INSERT INTO users (id, nick, ngs_avatar_url, kind)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE
		   SET nick = excluded.nick, ngs_avatar_url = excluded.ngs_avatar_url
		 WHERE users.kind = $4
		   AND users.anonymized_at IS NULL
		   AND (users.nick <> excluded.nick OR users.ngs_avatar_url <> excluded.ngs_avatar_url)`,
		a.ID, a.Nick, a.AvatarURL, KindShadow)
	if err != nil {
		return 0, fmt.Errorf("тень автора %d: %w", a.ID, err)
	}
	return a.ID, nil
}

// CreateNativeUser заводит пользователя без анкеты НГС — вход по инвайту, когда
// анкету снесли или сайта уже нет. Идентификатор берётся из нативной полосы.
func (p *Platform) CreateNativeUser(ctx context.Context, nick string) (int64, error) {
	var id int64
	err := p.pool.QueryRow(ctx, `
		INSERT INTO users (id, nick, kind)
		VALUES (nextval('users_native_seq'), $1, $2)
		RETURNING id`, nick, KindMember).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("создание пользователя %q: %w", nick, err)
	}
	return id, nil
}

// Promote переводит тень в участники: человек доказал, что анкета его.
// Идемпотентна.
func (p *Platform) Promote(ctx context.Context, id int64) error {
	tag, err := p.pool.Exec(ctx, `
		UPDATE users SET kind = $2 WHERE id = $1 AND kind = $3`, id, KindMember, KindShadow)
	if err != nil {
		return fmt.Errorf("перевод %d в участники: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		// Либо уже участник, либо строки нет вовсе — второе должно быть слышно.
		if _, err := p.UserByID(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// SetNick меняет ник. Действует ретроспективно везде, включая префиксы «Ник, »
// в чужих ответах: они дорисовываются из текущего ника, а не хранятся в телах.
func (p *Platform) SetNick(ctx context.Context, id int64, nick string) error {
	tag, err := p.pool.Exec(ctx, `UPDATE users SET nick = $2 WHERE id = $1`, id, nick)
	if err != nil {
		return fmt.Errorf("смена ника %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("пользователь %d: %w", id, ErrNotFound)
	}
	return nil
}

// SetAvatar привязывает к пользователю аватар из хранилища медиа.
//
// Условие «уже не тот» — не украшение: зеркало приносит аватар с КАЖДЫМ
// комментарием, и без него строка человека переписывалась бы на каждую реплику,
// то есть на пустом месте пухли бы и WAL, и сама таблица.
func (p *Platform) SetAvatar(ctx context.Context, id int64, sha []byte) error {
	_, err := p.pool.Exec(ctx, `
		UPDATE users SET avatar_sha = $2
		 WHERE id = $1 AND avatar_sha IS DISTINCT FROM $2`, id, sha)
	return wrapf(err, "аватар пользователя %d", id)
}

// Touch отмечает, что человек заходил. Пишется огрублённо — раз в час: строка
// пользователя иначе переписывается на каждый запрос страницы, а это мусор в
// WAL и распухание таблицы ради минуты точности, которая никому не нужна.
func (p *Platform) Touch(ctx context.Context, id int64, at time.Time) error {
	_, err := p.pool.Exec(ctx, `
		UPDATE users SET last_seen_at = $2
		 WHERE id = $1 AND (last_seen_at IS NULL OR last_seen_at < $2 - interval '1 hour')`, id, at)
	return wrapf(err, "отметка визита %d", id)
}

func wrapf(err error, format string, args ...any) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf(format+": %w", append(args, err)...)
}
