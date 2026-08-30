package platform

// Страница участника: карточка и то, что он написал.
//
// Заводилась она под жителей (эпик «народ»: у персонажа есть биография, и
// показать её было негде), но получилась общей — и это правильно: две страницы
// про человека, одна для живых и одна для придуманных, разошлись бы на первом же
// новом поле, а различие между ними и так называется на самой странице.
//
// ЧЕГО ЗДЕСЬ НЕТ НАМЕРЕННО. Последнего визита: огонёк «онлайн» эпик E не
// переносил с НГС сознательно, и «был здесь час назад» — то же сведение,
// набранное словами. Анонимных заметок: они не идут ни в счётчик, ни в список,
// иначе страница автора деанонимизировала бы его соседством чисел — тот же
// довод, по которому анонимы не попадают в «новые лица» дайджеста. Скрытого
// модератором: страница отвечает на вопрос «что этот человек сказал на виду», а
// для скрытого есть очередь.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// pubLimit — сколько публикаций показываем. Двадцать, как на странице ленты:
// это «чем человек занят», а не выгрузка — полную историю отдаёт
// `platform export`, и отдаёт её самому человеку.
const pubLimit = 20

// pubExcerptRunes — длина показанного начала. Реплики короткие (медиана 75
// знаков), а заметка бывает на страницу; двести рун — это примерно три строки:
// достаточно, чтобы узнать разговор, и мало, чтобы страница стала лентой.
const pubExcerptRunes = 200

// Profile — карточка участника.
type Profile struct {
	ID        int64
	Nick      string
	AvatarURL string
	Gender    Gender
	Kind      Kind
	Role      Role
	// Persona — реплики этого автора пишет машина. Показывается прямо на
	// странице: читатель вправе знать, с кем говорит. Справка (/help#narod) имён
	// не называет намеренно — список из десяти ников устареет через неделю, а
	// признак у анкеты не устареет.
	Persona      bool
	Bio          string
	CreatedAt    time.Time
	AnonymizedAt *time.Time
	BannedUntil  *time.Time
	BanReason    string
	Notes        int
	Comments     int
}

// Shadow — человека мы видели только через зеркало, сам он сюда не входил.
func (p Profile) Shadow() bool { return p.Kind == KindShadow }

// Banned — публикации запрещены на момент at.
func (p Profile) Banned(at time.Time) bool {
	return p.BannedUntil != nil && p.BannedUntil.After(at)
}

// PubNote и PubComment — строки «что человек написал». Не NoteView и не
// CommentView: тем нужны дерево, реакции, картинки и права смотрящего, а здесь
// нужны ссылка, дата и начало текста. Тащить сюда полные виды значило бы тащить
// и их запросы.
type PubNote struct {
	ID      int64
	At      time.Time
	Exact   bool
	Excerpt string
	Stage   bool
}

type PubComment struct {
	ID     int64
	NoteID int64
	At     time.Time
	// Excerpt — начало реплики, Note — начало заметки, в которой она сказана.
	// Второе обязательно: реплика вне разговора нечитаема, а один номер заметки
	// не говорит человеку ничего.
	Excerpt string
	Note    string
}

// profileQuery — карточка вместе со счётчиками, одним походом в базу.
//
// Счётчики подзапросами, а не отдельными вызовами: три round-trip на страницу
// одного человека — это втрое больше поводов подождать занятый пул, а пул на
// хосте общий с зеркалом.
//
// Оба идут по частичным индексам (notes_author и comments_author_time, оба
// WHERE status = 0). У самого говорливого участника площадки 138 тысяч реплик,
// и без индекса этот счёт был бы тем самым проходом на пятьдесят секунд, из-за
// которого обезличивание живёт командой, а не кнопкой.
const profileQuery = `
	SELECT u.id, u.nick, u.avatar_sha, m.mime, u.gender, u.kind, u.role, u.persona,
	       u.bio, u.created_at, u.anonymized_at, u.banned_until, u.ban_reason,
	       (SELECT count(*) FROM notes n
	         WHERE n.author_id = u.id AND n.status = 0 AND NOT n.anonymous),
	       (SELECT count(*) FROM comments c
	         WHERE c.author_id = u.id AND c.status = 0)
	  FROM users u
	  LEFT JOIN media m ON m.sha256 = u.avatar_sha
	 WHERE u.id = $1`

// UserProfile — карточка участника для его страницы.
func (p *Platform) UserProfile(ctx context.Context, id int64) (Profile, error) {
	var (
		v    Profile
		sha  []byte
		mime *string
	)
	err := p.pool.QueryRow(ctx, profileQuery, id).Scan(
		&v.ID, &v.Nick, &sha, &mime, &v.Gender, &v.Kind, &v.Role, &v.Persona,
		&v.Bio, &v.CreatedAt, &v.AnonymizedAt, &v.BannedUntil, &v.BanReason,
		&v.Notes, &v.Comments)
	if errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, fmt.Errorf("участник %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Profile{}, fmt.Errorf("участник %d: %w", id, err)
	}
	v.AvatarURL = MediaURL(sha, strOf(mime))
	return v, nil
}

// authorNotesQuery — заметки человека, свежие сверху.
//
// Вынесен в константу по той же причине, что feedQuery: тест спрашивает у
// Postgres план РОВНО этого SQL и требует notes_author. Порядок по времени, а не
// по id: полосы идентификаторов между собой по времени не упорядочены, и у
// человека с репликами 2010 года первой строкой пошла бы именно она.
const authorNotesQuery = `
	SELECT n.id, n.published_at, n.published_exact, left(n.body, 400), n.stage
	  FROM notes n
	 WHERE n.author_id = $1 AND n.status = 0 AND NOT n.anonymous
	 ORDER BY n.published_at DESC
	 LIMIT $2`

// AuthorNotes — последние заметки участника.
func (p *Platform) AuthorNotes(ctx context.Context, id int64, limit int) ([]PubNote, error) {
	rows, err := p.pool.Query(ctx, authorNotesQuery, id, clampPub(limit))
	if err != nil {
		return nil, fmt.Errorf("заметки участника %d: %w", id, err)
	}
	defer rows.Close()

	var out []PubNote
	for rows.Next() {
		var n PubNote
		if err := rows.Scan(&n.ID, &n.At, &n.Exact, &n.Excerpt, &n.Stage); err != nil {
			return nil, fmt.Errorf("заметки участника %d: %w", id, err)
		}
		n.Excerpt = excerpt(n.Excerpt)
		out = append(out, n)
	}
	return out, wrapf(rows.Err(), "заметки участника %d", id)
}

// authorCommentsQuery — реплики человека, свежие сверху.
//
// Заметка присоединяется ради её начала: реплика вне разговора нечитаема. Это
// поиск по первичному ключу на каждую из двадцати строк, то есть цена ровно
// двадцати обращений к notes.
//
// n.status = 0 обязательно: скрытая заметка уносит со страниц весь тред, и
// реплика из неё вела бы отсюда на пустое место.
const authorCommentsQuery = `
	SELECT c.id, c.note_id, c.published_at, left(c.body, 400), left(n.body, 400)
	  FROM comments c
	  JOIN notes n ON n.id = c.note_id
	 WHERE c.author_id = $1 AND c.status = 0 AND n.status = 0
	 ORDER BY c.published_at DESC
	 LIMIT $2`

// AuthorComments — последние реплики участника.
func (p *Platform) AuthorComments(ctx context.Context, id int64, limit int) ([]PubComment, error) {
	rows, err := p.pool.Query(ctx, authorCommentsQuery, id, clampPub(limit))
	if err != nil {
		return nil, fmt.Errorf("реплики участника %d: %w", id, err)
	}
	defer rows.Close()

	var out []PubComment
	for rows.Next() {
		var c PubComment
		if err := rows.Scan(&c.ID, &c.NoteID, &c.At, &c.Excerpt, &c.Note); err != nil {
			return nil, fmt.Errorf("реплики участника %d: %w", id, err)
		}
		c.Excerpt, c.Note = excerpt(c.Excerpt), excerpt(c.Note)
		out = append(out, c)
	}
	return out, wrapf(rows.Err(), "реплики участника %d", id)
}

func clampPub(limit int) int {
	if limit <= 0 || limit > pubLimit {
		return pubLimit
	}
	return limit
}

// excerpt — начало текста для списка.
//
// Переносы строк схлопываются в пробел: в списке строка одна, а заметка сплошь
// и рядом начинается с пустых строк — без этого первые строки списка оказались
// бы пустыми. Режется по РУНАМ (SQL отдал знаки), по границе слова, и обрезанное
// честно помечается многоточием: без него читатель принимает урезанную реплику
// за целую.
func excerpt(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= pubExcerptRunes {
		return s
	}
	cut := string(r[:pubExcerptRunes])
	if i := strings.LastIndex(cut, " "); i > len(cut)/2 {
		cut = cut[:i]
	}
	return cut + "…"
}
