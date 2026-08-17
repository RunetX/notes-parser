package platsink

// Перевод зеркальных строк (store) в приём площадки (platform). Место одно на
// оба входа — живой приёмник и сверку, — иначе они разошлись бы в мелочах, а
// расхождение видно только на живых данных и годы спустя.

import (
	"strconv"
	"time"

	"lovegw/internal/love"
	"lovegw/internal/platform"
	"lovegw/internal/store"
)

// noteFrom переводит заметку зеркала в приём.
//
// published_exact = false всегда: сайт в ленте времени публикации не даёт, и в
// lovegw.db лежит момент, когда заметку УВИДЕЛО зеркало. Врать точностью здесь
// нельзя — на этом поле стоит показ даты на странице.
func noteFrom(n store.Note) (platform.MirroredNote, error) {
	id, err := siteID(n.ID)
	if err != nil {
		return platform.MirroredNote{}, err
	}
	author := profileID(n.AuthorID)
	return platform.MirroredNote{
		ID: id,
		Author: platform.MirroredAuthor{
			ID:        author,
			Nick:      n.AuthorName,
			AvatarURL: avatarURL(n.AuthorAvatarURL),
		},
		// Аноним на НГС приходит без анкеты (author_id = «0»), и
		// деанонимизировать его нечем — в отличие от нашей будущей анонимки,
		// где настоящий автор хранится.
		Anonymous:      author == 0,
		Body:           n.Text,
		PublishedAt:    n.FirstSeenAt,
		PublishedExact: false,
		CommentsClosed: n.CommentsClosed,
	}, nil
}

// commentFrom переводит комментарий зеркала в приём. replyToID — id нашего же
// комментария-адресата, уже найденного по обращению «Ник, …».
//
// Префикс срезается из тела ТОЛЬКО когда адресат нашёлся: тогда он превращается
// в ребро и дорисовывается на показе из текущего ника. Не нашёлся (человек в
// этой заметке не писал, это автор заметки или ник не разошёлся) — текст
// остаётся как есть: без ребра снятое обращение исчезло бы совсем, а «кому
// отвечали» — это содержание реплики, а не оформление.
//
// Из этого следует правило для Ш6: reply_scan, проставляя ребро по мобильному
// дереву, обязан снять префикс тем же вызовом. Иначе на странице выйдет
// «Ник, Ник, …» — один раз из ребра, второй из тела.
func commentFrom(noteID int64, c store.Comment, replyToID int64) platform.MirroredComment {
	body, source := c.Text, platform.ReplyNone
	if replyToID != 0 {
		body, source = love.TrimAddressPrefix(c.Text), platform.ReplyPrefix
	}
	return platform.MirroredComment{
		ID:     c.ID,
		NoteID: noteID,
		Author: platform.MirroredAuthor{
			ID:        love.ProfileIDFromLink(c.AuthorLink),
			Nick:      c.AuthorName,
			AvatarURL: avatarURL(c.AvatarURL),
		},
		Body:        body,
		ReplyToID:   replyToID,
		ReplySource: source,
		PublishedAt: publishedAt(c),
	}
}

// profileID разбирает id анкеты автора заметки. «0» и пустое — анонимная
// заметка; нечисловое значение тоже читается как аноним: единственный источник
// этого поля — ссылка на анкету в разметке, и её отсутствие как раз и означает,
// что автора не показали.
func profileID(s string) int64 {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || !platform.IsNGS(id) {
		return 0
	}
	return id
}

// avatarURL — ссылка, по которой аватар можно забрать заново. Силуэт по
// умолчанию сюда не попадает: качать его потом незачем.
func avatarURL(url string) string {
	if !love.IsRealAvatar(url) {
		return ""
	}
	return url
}

// publishedAt — время реплики. У сайта оно есть почти всегда, но не всегда
// разбирается; тогда берём момент, когда комментарий увидело зеркало — колонка
// в Postgres NOT NULL, и «неизвестно» в ней не выразить.
func publishedAt(c store.Comment) time.Time {
	if !c.PublishedAt.IsZero() {
		return c.PublishedAt
	}
	return c.CreatedAt
}
