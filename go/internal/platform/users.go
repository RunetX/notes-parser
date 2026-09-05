package platform

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	anonymized_at, banned_until, ban_reason, created_at, last_seen_at`

// userDest — куда ложатся колонки userColumns, ровно в их порядке. Список живёт
// ОДНИМ значением, а не повторяется у каждого запроса, и оплачено это входом:
// у SessionUser была вторая, дописанная от руки копия, добавленная в Ш7
// ban_reason в неё не попала — и pgx честно ответил «13 полей против 12» на
// КАЖДЫЙ запрос вошедшего. Площадка при этом не сломалась заметно, а тихо
// назначила гостями всех сразу (18.08.2026).
//
// Соответствие списков стережёт TestКолонкиПользователяСходятсяСоСканом: он
// сравнивает длины, а не память.
func userDest(u *User) []any {
	return []any{&u.ID, &u.Nick, &u.AvatarSHA, &u.NGSAvatarURL, &u.Kind, &u.Role,
		&u.AnonymizedAt, &u.BannedUntil, &u.BanReason, &u.CreatedAt, &u.LastSeenAt}
}

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(userDest(&u)...)
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

// CreatePersonaUser заводит ЖИТЕЛЯ — анкету, реплики которой пишет машина
// (эпик «народ»).
//
// Устроен он как обычный нативный участник плюс признак, и это не экономия:
// житель обязан быть неотличим от участника ВЕЗДЕ, кроме двух мест — согласий
// (он их не подписывает, см. consentGuard) и песочницы (ему в ней можно
// писать). Заведи мы третий kind — и различие пришлось бы поддерживать в
// десятке запросов, где оно не значит ничего, а забытый там житель однажды
// выпал бы из ленты или из очереди модерации.
//
// В очередь модерации его реплики идут наравне со всеми, и это тоже намеренно:
// автомат — второй забор, а не украшение, и молчаливое исключение жителя из-под
// него означало бы, что на площадке есть автор, которого никто не проверяет.
func (p *Platform) CreatePersonaUser(ctx context.Context, nick string) (int64, error) {
	var id int64
	err := p.pool.QueryRow(ctx, `
		INSERT INTO users (id, nick, kind, persona)
		VALUES (nextval('users_native_seq'), $1, $2, true)
		RETURNING id`, nick, KindMember).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("создание жителя %q: %w", nick, err)
	}
	return id, nil
}

// ErrNoSystemUser — служебной анкеты площадки в базе нет. Ошибка отдельная и
// говорит, ЧТО делать: заводит строку `platform migrate`, и молчаливое
// «пользователь не найден» посреди публикации выпуска отправило бы искать её в
// конфиге, где её больше нет.
var ErrNoSystemUser = errors.New("служебной анкеты площадки нет: накатите platform migrate")

// systemUserQuery — служебная анкета площадки. Число KindService подставлено В
// ТЕКСТ, а не параметром, намеренно: под условие `kind = 2` подходит частичный
// уникальный индекс users_system (миграция 0026), а по параметру планировщик
// доказать этого не может и пошёл бы перебором сотен тысяч строк. Тот же приём,
// что у notStageNote в platdigest.
var systemUserQuery = fmt.Sprintf(`SELECT id FROM users WHERE kind = %d`, KindService)

// SystemUserID — номер служебной анкеты площадки.
func (p *Platform) SystemUserID(ctx context.Context) (int64, error) {
	var id int64
	err := p.pool.QueryRow(ctx, systemUserQuery).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNoSystemUser
	}
	if err != nil {
		return 0, fmt.Errorf("служебная анкета площадки: %w", err)
	}
	return id, nil
}

// EnsureSystemUser заводит АНКЕТУ САМОЙ ПЛОЩАДКИ и возвращает её номер.
// Идемпотентна.
//
// Нужна она затем, что у площадки есть СВОИ публикации — недельный выпуск, а в
// будущем и объявления, — и до 05.09.2026 их подписывал живой человек, владелец.
// Стоило это выноса выпуска на love.ngs.ru под его именем: галочка «отправлять
// мои записи на НГС» стои́т у автора, а не у текста, и отличить сводку площадки
// от заметки человека было нечем. Своя подпись отвечает на это структурно —
// enqueueNGS спрашивает `kind = KindMember`, и служебная анкета не проходит
// сама, без единого условия про дайджест.
//
// Роль МОДЕРАТОРСКАЯ, и ровно по двум делам: закрепить свой выпуск наверху
// ленты (SetNotePinned требует CanModerate) и не встать самой себе в очередь
// автомата (enqueueCheck пропускает role >= RoleModerator). Администратором её
// делать незачем — прав администратора ей не нужно ни для чего, а анкета,
// в которую нельзя войти, но которой всё можно, однажды станет дверью.
// Войти в неё и правда нельзя: вход по коду сверяет номер анкеты НГС, а он
// лежит ниже нативной полосы, откуда взят её id.
//
// Ник — latest-wins, как у тени: площадка зовётся одной константой (web.SiteName),
// и переименование обязано доезжать до подписи прошлых выпусков само. Имя
// приходит ПАРАМЕТРОМ, потому что константа живёт в морде, а второе её написание
// здесь разошлось бы с первым молча.
func (p *Platform) EnsureSystemUser(ctx context.Context, nick string) (int64, error) {
	nick = strings.TrimSpace(nick)
	if nick == "" {
		return 0, ErrBadNick
	}
	id, err := p.SystemUserID(ctx)
	switch {
	case err == nil:
		if _, err := p.pool.Exec(ctx,
			`UPDATE users SET nick = $2 WHERE id = $1 AND nick <> $2`, id, nick); err != nil {
			return 0, fmt.Errorf("переименование служебной анкеты: %w", err)
		}
		return id, nil
	case !errors.Is(err, ErrNoSystemUser):
		return 0, err
	}
	if err := p.pool.QueryRow(ctx, `
		INSERT INTO users (id, nick, kind, role)
		VALUES (nextval('users_native_seq'), $1, $2, $3)
		RETURNING id`, nick, KindService, RoleModerator).Scan(&id); err != nil {
		return 0, fmt.Errorf("создание служебной анкеты %q: %w", nick, err)
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

// SetNGSAvatar ставит фото, за которым сходили в анкету НГС НАРОЧНО: и байты
// (sha), и ссылку, откуда они взяты, — одной строкой.
//
// Отдельно от SetAvatar, потому что случай обратный. Зеркало приносит аватар
// вместе с комментарием, и ссылку менять ему не с чего; здесь же поводом была
// смена фото в анкете, и оставшаяся старая ссылка означала бы, что повторная
// закачка (MissingAvatars) целится в прежний файл.
//
// Обезличенного не трогаем вовсе: возврат его фото отменял бы исполненное
// требование субъекта — то же правило, что у ника в ensureShadow.
func (p *Platform) SetNGSAvatar(ctx context.Context, id int64, sha []byte, url string) error {
	tag, err := p.pool.Exec(ctx, `
		UPDATE users SET avatar_sha = $2, ngs_avatar_url = $3
		 WHERE id = $1 AND anonymized_at IS NULL`, id, sha, url)
	if err != nil {
		return wrapf(err, "фото пользователя %d", id)
	}
	if tag.RowsAffected() == 0 {
		u, err := p.UserByID(ctx, id) // ErrNotFound уходит наверх как есть
		if err != nil {
			return err
		}
		if u.AnonymizedAt != nil {
			return ErrAnonymized
		}
	}
	return nil
}

// ClearAvatar снимает фото: и байты, и ссылку, откуда они взяты.
//
// Понадобилось из тупика. «Обновить аватар» пустую анкету НГС за причину снять
// фото не считает — и правильно, снимать по ЧУЖОЙ руке нечестно: файлов
// площадка не принимает, и вернуть снятое было бы неоткуда. Но человек, стёрший
// фото В АНКЕТЕ, оставался здесь с прежним навсегда: на НГС его уже нет, само
// оно сюда не приедет никогда, а кнопка отвечает «всё осталось как было»
// (жалоба владельца, 28.08.2026). Здесь дорога есть, и решает по ней сам
// человек.
//
// ФАЙЛ ИЗ ХРАНИЛИЩА НЕ ТРОГАЕТСЯ ВОВСЕ. Имя файла есть его содержимое, поэтому
// на ту же строку media ссылаются и чужие: одна и та же картинка у нескольких
// анкет — обычное дело, и удаление ради нескольких килобайт оставило бы
// соседям битую картинку.
//
// Ссылка снимается ВМЕСТЕ с байтами, и это не аккуратность: разовый добор медиа
// (MissingAvatars) берёт как раз тех, у кого ссылка есть, а байтов нет, —
// оставив её, мы своей же рукой велели бы вернуть снятое фото на место.
//
// Чего это не обещает: пока фото стоит в анкете НГС, его вернёт первый же
// приехавший оттуда комментарий — зеркало приносит аватар вместе с репликой
// (platsink.putAvatar). Насовсем убирают там же, где поставили.
func (p *Platform) ClearAvatar(ctx context.Context, id int64) error {
	tag, err := p.pool.Exec(ctx, `
		UPDATE users SET avatar_sha = NULL, ngs_avatar_url = ''
		 WHERE id = $1`, id)
	if err != nil {
		return wrapf(err, "снятие фото пользователя %d", id)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("пользователь %d: %w", id, ErrNotFound)
	}
	return nil
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

// SetGenders проставляет пол участникам. Приезжает он даром — со страницы
// комментариев, по которой и так идёт обход дерева, — поэтому отдельного
// обхода анкет (как у `personas gender`) не нужно.
//
// Обезличенных не трогаем: возврат пола из зеркала отменял бы исполненное
// требование субъекта, ровно как и возврат ника.
func (p *Platform) SetGenders(ctx context.Context, byID map[int64]Gender) (int, error) {
	changed := 0
	for id, v := range byID {
		if v == GenderUnknown {
			continue
		}
		tag, err := p.pool.Exec(ctx, `
			UPDATE users SET gender = $2
			 WHERE id = $1 AND gender <> $2 AND anonymized_at IS NULL`, id, v)
		if err != nil {
			return changed, fmt.Errorf("пол участника %d: %w", id, err)
		}
		changed += int(tag.RowsAffected())
	}
	return changed, nil
}
