package platform

// Доменные типы площадки и — главное — граница показа.
//
// Строки (User, Note, Comment) — это то, что лежит в базе, включая настоящего
// автора анонимной заметки. Виды (NoteView, CommentView) — то, что можно
// показать человеку, и автора анонимной публикации в них ПРОСТО НЕТ ПОЛЯ.
// Поэтому забыть скрыть автора нельзя структурно: маскирование живёт в SELECT
// (CASE WHEN anonymous), а тип вида не оставляет места для утечки даже при
// небрежном коде наверху.

import (
	"errors"
	"time"
)

// ErrNotFound — запрошенной строки нет.
var ErrNotFound = errors.New("запись не найдена")

// План идентификаторов (см. шапку migrations/0001_init.sql): id строки НГС равен
// id на сайте, нативные выдаются последовательностями с 1e11.
const (
	// NativeIDBase — начало полосы нативных идентификаторов.
	NativeIDBase int64 = 100_000_000_000
	// IDBandLimit — потолок занятых полос; выше зарезервировано.
	IDBandLimit int64 = 200_000_000_000
)

// IsNGS — идентификатор пришёл с НГС (и равен идентификатору на сайте).
func IsNGS(id int64) bool { return id > 0 && id < NativeIDBase }

// IsNative — идентификатор выдан нами.
func IsNative(id int64) bool { return id >= NativeIDBase && id < IDBandLimit }

// Kind — вид пользователя.
type Kind int16

const (
	KindShadow  Kind = 0 // тень: видели только через зеркало, сам не входил
	KindMember  Kind = 1 // участник: доказал владение анкетой или вошёл по инвайту
	KindService Kind = 2 // служебный
)

// Role — права.
type Role int16

const (
	RoleUser      Role = 0
	RoleModerator Role = 1
	RoleAdmin     Role = 2
)

// Status — видимость публикации.
type Status int16

const (
	StatusVisible     Status = 0
	StatusHiddenOwner Status = 1 // скрыл автор
	StatusHiddenMod   Status = 2 // скрыла модерация
	StatusAnonymized  Status = 3 // обезличено по требованию субъекта
)

// ReplySource — откуда известен адресат ответа. Источники названы честно,
// потому что их надёжность измерена и разная: префикс «Ник, …» совпадает с
// настоящим адресатом в 48 % случаев, мобильное дерево — в 92 %.
type ReplySource int16

const (
	ReplyNone          ReplySource = 0
	ReplyPrefix        ReplySource = 1 // из префикса «Ник, …»
	ReplyMobileTree    ReplySource = 2 // из мобильного дерева ответов
	ReplyNative        ReplySource = 3 // ответили у нас, адресат известен точно
	ReplyDesktopParent ReplySource = 4 // parent_id десктопа: это корень ветки, а не адресат
)

// Viewer — кто смотрит. Нулевое значение — не вошедший.
type Viewer struct {
	UserID int64
	Role   Role
}

// CanModerate — виден ли смотрящему инструмент модерации. Права на ПОКАЗ скрытого
// это не даёт: автор анонимной публикации не раскрывается никому, включая
// модератора, — забанить флудера можно, не зная, кто он.
func (v Viewer) CanModerate() bool { return v.Role >= RoleModerator }

// ---------------------------------------------------------------- строки базы

// User — строка users. И участник, и тень: разница в Kind.
type User struct {
	ID           int64
	Nick         string // ТЕКУЩИЙ ник, latest-wins; истории ников нет
	AvatarSHA    []byte
	NGSAvatarURL string // откуда взяли аватар — для повторной закачки, не для показа
	Kind         Kind
	Role         Role
	HideAll      bool
	AnonymizedAt *time.Time
	BannedUntil  *time.Time
	CreatedAt    time.Time
	LastSeenAt   *time.Time
}

// Banned — человек забанен на момент at.
func (u User) Banned(at time.Time) bool {
	return u.BannedUntil != nil && u.BannedUntil.After(at)
}

// Note — строка notes. AuthorID у анонимной заметки НАСТОЯЩИЙ (0 только у
// зеркальных анонимов НГС, которых деанонимизировать нечем).
type Note struct {
	ID             int64
	AuthorID       int64
	Anonymous      bool
	Body           string
	Status         Status
	CommentsClosed bool
	CommentCount   int
	PublishedAt    time.Time
	PublishedExact bool
	LastCommentAt  *time.Time
	EditedAt       *time.Time
	CreatedAt      time.Time
}

// Comment — строка comments. Body хранится БЕЗ префикса «Ник, »: он снят в
// ребро на приёме, иначе ник размазан по чужим телам и обезличивание невозможно.
type Comment struct {
	ID            int64
	NoteID        int64
	AuthorID      int64
	AuthorDisplay string // снимок ника безанкетного комментатора зеркала
	Anonymous     bool
	Body          string
	BranchRootID  int64
	ReplyToID     int64
	ReplySource   ReplySource
	Path          string
	Depth         int16
	Status        Status
	PublishedAt   time.Time
	EditedAt      *time.Time
	CreatedAt     time.Time
}

// ---------------------------------------------------------------- виды показа

// Gender — пол участника. Красит ник, как на НГС; у безанкетных комментаторов
// зеркала неизвестен.
type Gender int16

const (
	GenderUnknown Gender = 0
	GenderMale    Gender = 1
	GenderFemale  Gender = 2
)

// Class — класс ника для разметки: те же `_male` / `_female`, что на сайте.
func (g Gender) Class() string {
	switch g {
	case GenderMale:
		return "_male"
	case GenderFemale:
		return "_female"
	default:
		return ""
	}
}

// Author — всё, что о человеке можно показать. AvatarURL — ТОЛЬКО наш путь
// /media/…: ссылки на hsmedia.ru наружу не уходят, иначе смерть НГС забирает с
// собой и наши страницы (и заодно сообщает ему каждого нашего читателя).
type Author struct {
	ID        int64
	Nick      string
	AvatarURL string
	Gender    Gender
}

// Known — за строкой есть анкета (а не безанкетный комментатор и не аноним).
func (a Author) Known() bool { return a.ID != 0 }

// NoteView — заметка для показа. Поля «настоящий автор анонимки» тут нет.
type NoteView struct {
	ID             int64
	Anonymous      bool
	Author         Author // нулевой у анонимной
	Body           string
	Status         Status
	CommentsClosed bool
	CommentCount   int
	PublishedAt    time.Time
	// PublishedExact = false означает, что в PublishedAt лежит момент, когда
	// заметку увидело зеркало: настоящего времени публикации сайт не даёт.
	PublishedExact bool
	LastCommentAt  *time.Time
	EditedAt       *time.Time
	Own            bool // «моя» — чтобы автор видел свою анонимку среди своих
}

// Name — подпись под заметкой. Пара к CommentView.Name: подпись обязана
// считаться в одном месте, иначе аноним где-нибудь окажется «без имени».
//
// «Анонимно», а не «Аноним»: ровно это слово стоит под аватаром анонимной
// заметки на НГС (записанная лента, notes_feed.html).
func (n NoteView) Name() string {
	switch {
	case n.Anonymous:
		return "Анонимно"
	case n.Author.Known() && n.Author.Nick != "":
		return n.Author.Nick
	default:
		return "Без имени"
	}
}

// ReplyRef — адресат ответа для дорисовки префикса «Ник, …». Ник берётся
// ТЕКУЩИЙ, поэтому переименование и обезличивание меняют его и в чужих ответах.
type ReplyRef struct {
	CommentID int64
	Nick      string
	Anonymous bool
}

// CommentView — комментарий для показа.
type CommentView struct {
	ID          int64
	NoteID      int64
	Anonymous   bool
	Author      Author
	Display     string // ник безанкетного комментатора зеркала, если анкеты нет
	Body        string
	ReplyTo     *ReplyRef
	Path        string
	Depth       int
	Status      Status
	PublishedAt time.Time
	EditedAt    *time.Time
	Own         bool
}

// Name — подпись под комментарием: ник анкеты, снимок ника безанкетного или
// «Аноним». Одно место на все шаблоны, чтобы подпись не расходилась по видам.
func (c CommentView) Name() string {
	switch {
	case c.Anonymous:
		return "Аноним"
	case c.Author.Known():
		return c.Author.Nick
	case c.Display != "":
		return c.Display
	default:
		return "Без имени"
	}
}

// ---------------------------------------------------------------- приём зеркала

// MirroredAuthor — автор, каким его видно на НГС. ID = 0 означает, что анкеты за
// подписью нет: у зеркала это либо аноним, либо комментатор без ссылки на анкету.
type MirroredAuthor struct {
	ID        int64
	Nick      string
	AvatarURL string
}

// MirroredNote — заметка с НГС на приёме. ID равен id на сайте.
type MirroredNote struct {
	ID             int64
	Author         MirroredAuthor
	Anonymous      bool
	Body           string
	PublishedAt    time.Time
	PublishedExact bool
	CommentsClosed bool
}

// MirroredComment — комментарий с НГС на приёме.
//
// ReplyToID — id НАШЕГО комментария-адресата: зеркало уже считает его через
// love.AddressPrefix и store.AddresseeMessage, и для площадки «сообщение
// приёмника» это и есть id строки comments. Поэтому адресат достаётся даром, а
// не вычисляется второй раз.
type MirroredComment struct {
	ID          int64
	NoteID      int64
	Author      MirroredAuthor
	Body        string // уже без префикса «Ник, »
	ReplyToID   int64
	ReplySource ReplySource
	PublishedAt time.Time
}

// ---------------------------------------------------------------- запись у нас

// NewNote — нативная заметка.
type NewNote struct {
	AuthorID  int64
	Anonymous bool
	Body      string
}

// NewComment — нативный комментарий. ReplyToID = 0 — корень треда.
type NewComment struct {
	NoteID    int64
	AuthorID  int64
	Anonymous bool
	Body      string
	ReplyToID int64
}

// nullID переводит 0 в NULL: в базе «автора нет» это NULL, а в Go — 0.
func nullID(id int64) *int64 {
	if id == 0 {
		return nil
	}
	return &id
}

func idOf(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func strOf(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
