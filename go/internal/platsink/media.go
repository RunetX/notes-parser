package platsink

// Разовый добор медиа: пройти по строкам, где ссылка известна, а байтов нет, и
// забрать файлы с CDN сайта.
//
// Живой приёмник наполняет хранилище сам — байты приезжают вместе с потоком
// зеркала. Но исторические строки (всё, что легло бэкфиллом) знают только
// ссылку, и, главное, поток может пересохнуть раньше, чем наполнит хранилище:
// 17.08.2026 НГС восстановился, но комментировать так и не разрешил — новых
// реплик нет, а значит нет и аватаров. Ссылки при этом ещё живые.
//
// Отсюда правило: **это окно закрывается вместе с сайтом**. Пока hsmedia.ru
// отдаёт файлы, их надо забрать; после — на страницах площадки останутся пустые
// места, и взять их будет неоткуда.

import (
	"context"
	"fmt"
	"log/slog"

	"lovegw/internal/platform"
)

// Fetcher — то, что умеет скачать файл по ссылке. У демона это love.Client с
// его лимитером и RU-IP; в тестах — замыкание.
type Fetcher interface {
	FetchMedia(ctx context.Context, url string) ([]byte, error)
}

// MediaStats — итог обхода.
type MediaStats struct {
	Avatars int // аватаров забрано
	Images  int // иллюстраций забрано
	Failed  int // ссылок, по которым не вышло
}

// MediaSweep — обход недостающих файлов.
type MediaSweep struct {
	p     *platform.Platform
	media *platform.MediaStore
	site  Fetcher
	log   *slog.Logger
}

// NewMediaSweep создаёт обход.
func NewMediaSweep(p *platform.Platform, media *platform.MediaStore, site Fetcher, log *slog.Logger) *MediaSweep {
	if log == nil {
		log = slog.Default()
	}
	return &MediaSweep{p: p, media: media, site: site, log: log}
}

// Once забирает до limit аватаров и до limit иллюстраций.
//
// Битая ссылка — не повод останавливаться: аватар мог быть удалён вместе с
// анкетой, а обход из-за одной строки не должен бросать остальные. Отмену
// контекста, наоборот, уважаем сразу — иначе `-limit 5000` не прервать.
func (s *MediaSweep) Once(ctx context.Context, limit int) (MediaStats, error) {
	var st MediaStats

	avatars, err := s.p.MissingAvatars(ctx, limit)
	if err != nil {
		return st, err
	}
	for _, a := range avatars {
		sha, err := s.grab(ctx, a.URL)
		if err != nil {
			if ctx.Err() != nil {
				return st, ctx.Err()
			}
			st.Failed++
			s.log.Warn("аватар не забран", "user", a.ID, "url", a.URL, "err", err)
			continue
		}
		if err := s.p.SetAvatar(ctx, a.ID, sha); err != nil {
			return st, err
		}
		st.Avatars++
	}

	images, err := s.p.MissingNoteImages(ctx, limit)
	if err != nil {
		return st, err
	}
	for _, img := range images {
		sha, err := s.grab(ctx, img.URL)
		if err != nil {
			if ctx.Err() != nil {
				return st, ctx.Err()
			}
			st.Failed++
			s.log.Warn("иллюстрация не забрана", "note", img.ID, "url", img.URL, "err", err)
			continue
		}
		if err := s.p.AttachNoteImage(ctx, img.ID, sha, img.URL); err != nil {
			return st, err
		}
		st.Images++
	}
	return st, nil
}

// grab качает файл и кладёт его в хранилище. Хранилище само откажется принять
// не-картинку: геоблок DDoS-Guard отдаёт на запрос картинки HTML с кодом 200, и
// такой «аватар» осел бы у нас молча, а на странице оказался битым.
func (s *MediaSweep) grab(ctx context.Context, url string) ([]byte, error) {
	data, err := s.site.FetchMedia(ctx, url)
	if err != nil {
		return nil, err
	}
	m, err := s.media.Put(ctx, data, url)
	if err != nil {
		return nil, fmt.Errorf("хранилище: %w", err)
	}
	return m.SHA256, nil
}
