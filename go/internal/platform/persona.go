package platform

// Жители площадки со стороны ядра (эпик «народ»): биография, фото и список.
//
// Три операции, и все три существуют по ОДНОЙ причине — у персонажа нет анкеты
// НГС. Человеку фото приносит зеркало вместе с репликой, а «о себе» площадка не
// заводила ему нигде; житель же придуман оператором целиком, и всё, что видно на
// его странице, кто-то обязан положить сюда руками.

import (
	"context"
	"errors"
	"fmt"
)

// ErrNotPersona — так нельзя с анкетой живого человека.
//
// Отдельная ошибка, а не ErrNotAdmin: администратор здесь как раз вправе, дело
// в ОБЪЕКТЕ. Биографию и фото оператор сочиняет от себя, и то же действие над
// живым было бы подделкой — «о себе», которого человек не писал, и фото,
// которого он не выбирал.
var ErrNotPersona = errors.New("это анкета живого человека, а не жителя")

// ActionAvatar / ActionAvatarOff — администратор поставил жителю фото или снял
// его. Своё действие, а не ActionImage: та стоит у иллюстрации ЗАМЕТКИ, и
// читать журнал, в котором два разных решения названы одним словом, значит
// каждый раз доходить до объекта, чтобы понять, о чём строка.
const (
	ActionAvatar    = "avatar"
	ActionAvatarOff = "avatar_remove"
)

// SetPersonaBio — биография жителя, та самая, что видна на его странице.
//
// Без актора, как и CreatePersonaUser рядом: зовёт её `narod enroll`, то есть
// оператор из консоли, и второго пути сюда нет. Идемпотентна намеренно —
// карточку правят, и повторный enroll обязан донести правку до страницы.
//
// Проверка persona стоит ЗДЕСЬ, а не у вызывающего: колонка `bio` живёт в общей
// таблице users, и единственное, что отделяет её от поля «о себе» для живых
// людей, — вот этот отказ.
func (p *Platform) SetPersonaBio(ctx context.Context, id int64, bio string) error {
	tag, err := p.pool.Exec(ctx, `
		UPDATE users SET bio = $2 WHERE id = $1 AND persona`, id, bio)
	if err != nil {
		return wrapf(err, "биография жителя %d", id)
	}
	if tag.RowsAffected() == 0 {
		return p.whyNotPersona(ctx, id)
	}
	return nil
}

// SetPersonaAvatarAsAdmin — поставить жителю фото либо снять его (m == nil).
//
// Дверь администраторская по тому же доводу, что у песочницы: кто здесь
// ГОВОРИТ — вопрос ролей, а не слов, а лицо жителя есть часть того, кем его
// видят. Модератору она не достаётся: он решает про сказанное.
//
// ngs_avatar_url остаётся ПУСТЫМ, и это не небрежность. По нему `platform media`
// добирает байты тем, у кого ссылка есть, а файла нет, — у жителя же ссылки нет
// и быть не может: записав туда что-нибудь, мы завели бы вечную задачу на
// закачку с несуществующего адреса.
//
// Файл из хранилища при снятии НЕ трогается, как и у ClearAvatar: имя файла есть
// его содержимое, и на ту же картинку вправе ссылаться чужие строки.
func (p *Platform) SetPersonaAvatarAsAdmin(ctx context.Context, actor Viewer, id int64, m *Media, reason string) error {
	if !actor.CanAdmin() {
		return ErrNotAdmin
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return wrapf(err, "фото жителя %d", id)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	var sha []byte
	if m != nil {
		sha = m.SHA256
	}
	tag, err := tx.Exec(ctx, `
		UPDATE users SET avatar_sha = $2, ngs_avatar_url = '' WHERE id = $1 AND persona`, id, sha)
	if err != nil {
		return wrapf(err, "фото жителя %d", id)
	}
	if tag.RowsAffected() == 0 {
		return p.whyNotPersona(ctx, id)
	}
	action := ActionAvatar
	if m == nil {
		action = ActionAvatarOff
	}
	if err := audit(ctx, tx, actor.UserID, action,
		UserSubject(id), map[string]any{"reason": reason}); err != nil {
		return err
	}
	return wrapf(tx.Commit(ctx), "фото жителя %d", id)
}

// whyNotPersona объясняет, почему UPDATE не задел ни строки: анкеты нет вовсе
// или она принадлежит живому человеку. Разница видна только запросом, и спросить
// дешевле, чем вернуть одну ошибку на два разных случая.
func (p *Platform) whyNotPersona(ctx context.Context, id int64) error {
	if _, err := p.UserByID(ctx, id); err != nil {
		return err // ErrNotFound уходит наверх как есть
	}
	return ErrNotPersona
}

// PersonaRow — житель в списке администратора: ровно то, чем его там правят.
type PersonaRow struct {
	ID        int64
	Nick      string
	AvatarURL string
	Gender    Gender
	Bio       string
}

// Personas — все жители, по порядку заведения.
//
// Без счётчиков и без разбиения на страницы: их десятки, а страница, на которой
// они живут, — про фото, а не про то, кто сколько сказал. Сколько сказал, видно
// на самой странице жителя.
func (p *Platform) Personas(ctx context.Context) ([]PersonaRow, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT u.id, u.nick, u.avatar_sha, m.mime, u.gender, u.bio
		  FROM users u
		  LEFT JOIN media m ON m.sha256 = u.avatar_sha
		 WHERE u.persona
		 ORDER BY u.id`)
	if err != nil {
		return nil, fmt.Errorf("жители: %w", err)
	}
	defer rows.Close()

	var out []PersonaRow
	for rows.Next() {
		var (
			r    PersonaRow
			sha  []byte
			mime *string
		)
		if err := rows.Scan(&r.ID, &r.Nick, &sha, &mime, &r.Gender, &r.Bio); err != nil {
			return nil, fmt.Errorf("жители: %w", err)
		}
		r.AvatarURL = MediaURL(sha, strOf(mime))
		out = append(out, r)
	}
	return out, wrapf(rows.Err(), "жители")
}

// PersonaFace — житель в мордоленте: лицо, имя и номер, по которому собирается
// адрес страницы. Ни биографии, ни счётчиков: полоса отвечает на вопрос «кто
// здесь живёт», а всё остальное про человека — на его собственной странице.
type PersonaFace struct {
	ID        int64
	Nick      string
	AvatarURL string
	Gender    Gender
}

// personaFacesQuery — жители для мордоленты, недавно говорившие первыми.
//
// Условий два, и оба существенные. JOIN к media, а не LEFT: мордолента есть
// лента ЛИЦ, и житель без фото занял бы в ней место силуэтом, то есть сказал бы
// читателю ровно ничего. Порядок — по последнему сказанному слову: полоса, у
// которой порядок вечный, за неделю становится частью фона, а на НГС мордолента
// как раз двигалась (жалоба тех лет — «мордолента уже третьи сутки не
// двигается»). Ещё не заговоривший житель уходит в конец, но из полосы не
// пропадает: он здесь живёт, просто пока молчит.
//
// Оба индекса на месте: жителей отбирает частичный users_persona (миграция
// 0021), «когда говорил» отвечает comments_author_time (0011) — по одному
// обращению на жителя, а не проходом по 10,7 млн реплик. Условие status = 0 в
// подзапросе стоит не ради показа (времени наружу не отдаём), а ради ПЛАНА:
// индекс частичный по тому же условию, и без него подзапрос уходит в перебор.
const personaFacesQuery = `
	SELECT u.id, u.nick, u.avatar_sha, m.mime, u.gender
	  FROM users u
	  JOIN media m ON m.sha256 = u.avatar_sha
	 WHERE u.persona
	 ORDER BY (SELECT max(c.published_at) FROM comments c
	            WHERE c.author_id = u.id AND c.status = 0) DESC NULLS LAST, u.id
	 LIMIT $1`

// PersonaFaces — мордолента: жители с фотографией, недавно говорившие первыми.
//
// Читается на каждом показе первой страницы ленты, поэтому и запрос, и оба его
// индекса названы выше поимённо: это самый частый запрос площадки, и лишний
// перебор здесь отбирается у зеркала, живущего на том же ядре.
func (p *Platform) PersonaFaces(ctx context.Context, limit int) ([]PersonaFace, error) {
	rows, err := p.pool.Query(ctx, personaFacesQuery, limit)
	if err != nil {
		return nil, fmt.Errorf("мордолента: %w", err)
	}
	defer rows.Close()

	var out []PersonaFace
	for rows.Next() {
		var (
			f    PersonaFace
			sha  []byte
			mime *string
		)
		if err := rows.Scan(&f.ID, &f.Nick, &sha, &mime, &f.Gender); err != nil {
			return nil, fmt.Errorf("мордолента: %w", err)
		}
		f.AvatarURL = MediaURL(sha, strOf(mime))
		out = append(out, f)
	}
	return out, wrapf(rows.Err(), "мордолента")
}
