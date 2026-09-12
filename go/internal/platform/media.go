package platform

// Хранилище медиа — content-addressable: имя файла есть sha256 его содержимого.
//
// Отсюда три свойства даром. Один и тот же аватар, встреченный у тысячи
// комментариев, лежит на диске один раз. Содержимое по ссылке неизменно, поэтому
// Caddy отдаёт его с immutable на год. И запись идемпотентна: повторный приём
// того же файла — это проверка наличия, а не перезапись.
//
// Байты отдаёт Caddy напрямую из /srv/media, мимо Go: это самый жирный по
// трафику путь, а ядро на сервере одно.
//
// С 26.08.2026 у колонок два разных смысла, и их стоит держать в голове.
// note_images.url у ЗЕРКАЛЬНОЙ строки — чужой адрес на hsmedia.ru, откуда файл
// был взят; у строки, принесённой участником, — наш собственный путь /media/…,
// потому что колонка NOT NULL, а взять картинку больше неоткуда: её принесли.
// Отличаются они по media.source_url: у принесённой он пуст. По этой пустоте
// «своё» и отделяется от «привезённого», а не по виду адреса.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/gif"  // распознавание размеров
	_ "image/jpeg" // -//-
	_ "image/png"  // -//-
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// MediaURLPrefix — путь, по которому медиа отдаёт прокси.
const MediaURLPrefix = "/media/"

// Media — учётная запись файла.
type Media struct {
	SHA256    []byte
	MIME      string
	Bytes     int
	Width     int
	Height    int
	SourceURL string
	URL       string
}

// MediaStore — хранилище: каталог на диске плюс учёт в базе.
type MediaStore struct {
	p   *Platform
	dir string
}

// NewMediaStore создаёт хранилище и каталог под него.
func NewMediaStore(p *Platform, dir string) (*MediaStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("каталог медиа не задан")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("каталог медиа %s: %w", dir, err)
	}
	s := &MediaStore{p: p, dir: dir}
	// Платформа узнаёт о хранилище здесь, а не отдельным вызовом: забытый вызов
	// не падает, он молча оставляет уборку без рук.
	p.media = s
	return s, nil
}

// Dir — корень хранилища.
func (s *MediaStore) Dir() string { return s.dir }

// Put кладёт файл в хранилище и учитывает его в базе. Повторный вызов с тем же
// содержимым лишь обновляет отметку обращения.
//
// Не-картинку отказываемся принимать намеренно, и это не придирка к типам:
// геоблок DDoS-Guard отдаёт на запрос картинки HTML-страницу с кодом 200, и
// такой «аватар» осел бы в хранилище молча, а на страницах появился битым.
func (s *MediaStore) Put(ctx context.Context, data []byte, sourceURL string) (Media, error) {
	return s.PutSized(ctx, data, sourceURL, 0, 0)
}

// PutSized — то же самое, но с ЯВНЫМИ размерами.
//
// Нужен там, где формат выхода stdlib прочитать не умеет (webp), а размеры и так
// известны: их задавал тот, кто перекодировал. Декодировать файл ради двух
// чисел, лежащих в переменной, — работа ни за чем, а тянуть ради этого первую в
// проекте картиночную зависимость тем более.
//
// Нулевые w/h означают «посчитай сам» — этим и стал прежний Put.
func (s *MediaStore) PutSized(ctx context.Context, data []byte, sourceURL string, w, h int) (Media, error) {
	if len(data) == 0 {
		return Media{}, fmt.Errorf("пустой файл (%s)", sourceURL)
	}
	mime := detectMIME(data)
	if !strings.HasPrefix(mime, "image/") {
		return Media{}, fmt.Errorf("не картинка, а %s (%s)", mime, sourceURL)
	}
	sum := sha256.Sum256(data)
	sha := sum[:]

	m := Media{
		SHA256:    sha,
		MIME:      mime,
		Bytes:     len(data),
		SourceURL: sourceURL,
		URL:       MediaURL(sha, mime),
	}
	// Размеры — «по возможности»: webp и прочие форматы вне stdlib просто не
	// дадут их, и это не повод отказывать в приёме. Нужны они разметке, чтобы
	// страница не прыгала при загрузке картинок.
	m.Width, m.Height = w, h
	if m.Width <= 0 || m.Height <= 0 {
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
			m.Width, m.Height = cfg.Width, cfg.Height
		}
	}

	if err := s.write(sha, mime, data); err != nil {
		return Media{}, err
	}
	if _, err := s.p.pool.Exec(ctx, `
		INSERT INTO media (sha256, mime, bytes, width, height, source_url)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (sha256) DO UPDATE SET last_hit_at = now()`,
		sha, mime, m.Bytes, nullDim(m.Width), nullDim(m.Height), sourceURL); err != nil {
		return Media{}, fmt.Errorf("учёт медиа %s: %w", hex.EncodeToString(sha), err)
	}
	return m, nil
}

// write кладёт байты на диск. Уже лежащий файл не переписывается: имя — это его
// содержимое, переписывать нечем. Запись идёт через временный файл в том же
// каталоге и rename, иначе оборванная закачка оставила бы обрезанную картинку
// под правильным именем — то есть навсегда.
func (s *MediaStore) write(sha []byte, mime string, data []byte) error {
	path := s.FilePath(sha, mime)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("каталог медиа %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("временный файл медиа: %w", err)
	}
	defer os.Remove(tmp.Name()) // после успешного rename это no-op

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("запись медиа: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("сброс медиа на диск: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("закрытие медиа: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return fmt.Errorf("права на медиа: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		// Гонка двух приёмов одного файла: кто-то положил его первым, и это
		// ровно тот же файл — содержимое задаёт имя.
		if _, statErr := os.Stat(path); statErr == nil {
			return nil
		}
		return fmt.Errorf("перенос медиа: %w", err)
	}
	return nil
}

// FilePath — путь файла на диске. Раскладка по двум первым знакам sha, чтобы в
// одном каталоге не оказалось десятков тысяч записей.
func (s *MediaStore) FilePath(sha []byte, mime string) string {
	h := hex.EncodeToString(sha)
	return filepath.Join(s.dir, h[:2], h+mediaExt(mime))
}

// Read — байты файла из хранилища.
//
// Единственный читатель — «сделать аватаром» (эпик M): уже принятую фотографию
// надо уменьшить, а уменьшает её морда, у которой есть перекодировщик. Публичнее
// от этого файл не становится — он и так лежит по открытому адресу, — здесь
// просто короткая дорога к тем же байтам, минуя свой же HTTP.
//
// Потолок не нужен: в хранилище попадает только то, что мы сами перекодировали,
// и размер известен из media.
func (s *MediaStore) Read(m Media) ([]byte, error) {
	data, err := os.ReadFile(s.FilePath(m.SHA256, m.MIME))
	if err != nil {
		return nil, fmt.Errorf("файл %s: %w", MediaURL(m.SHA256, m.MIME), err)
	}
	return data, nil
}

// Has — файл уже в хранилище (проверяется диск, а не база: правда — на диске).
func (s *MediaStore) Has(sha []byte, mime string) bool {
	if len(sha) == 0 {
		return false
	}
	_, err := os.Stat(s.FilePath(sha, mime))
	return err == nil
}

// AttachNoteImage привязывает иллюстрацию к заметке. sha пуст — ссылку знаем, а
// байты ещё не забрали (заберём позже; наружу ссылка на hsmedia.ru не уходит).
//
// Ключ — ссылка, а позиция берётся следующей свободной. Так сделано потому, что
// писателей двое и номера они знают по-разному: у живого зеркала порядкового
// номера нет вовсе (в mirror.Sink его не передают), а сверка приходит со своим
// из lovegw.db. Порядок при этом сохраняется: обе стороны привязывают картинки
// заметки в порядке показа.
func (p *Platform) AttachNoteImage(ctx context.Context, noteID int64, sha []byte, url string) error {
	var shaArg any
	if len(sha) > 0 {
		shaArg = sha
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO note_images (note_id, position, sha256, url)
		SELECT $1, coalesce(max(position) + 1, 0), $2, $3
		  FROM note_images WHERE note_id = $1
		ON CONFLICT (note_id, url) DO UPDATE
		   SET sha256 = coalesce(excluded.sha256, note_images.sha256)`,
		noteID, shaArg, url)
	return wrapf(err, "иллюстрация заметки %d (%s)", noteID, url)
}

// MissingMedia — строка, у которой ссылка известна, а байтов нет. ID — человек
// (аватар) или заметка (иллюстрация).
type MissingMedia struct {
	ID  int64
	URL string
}

// MissingAvatars — люди, чей аватар мы видели ссылкой, но не забрали.
//
// Нужны отдельным обходом, потому что байты приезжают только живым потоком
// зеркала: у исторических строк их взять неоткуда. Пока НГС жив, ссылки
// работают, и это окно надо использовать — оно закрывается вместе с сайтом.
func (p *Platform) MissingAvatars(ctx context.Context, limit int) ([]MissingMedia, error) {
	return p.missingMedia(ctx, "аватары без байтов", `
		SELECT id, ngs_avatar_url FROM users
		 WHERE ngs_avatar_url <> '' AND avatar_sha IS NULL
		 ORDER BY id
		 LIMIT $1`, limit)
}

// MissingNoteImages — иллюстрации заметок, привязанные ссылкой без байтов.
func (p *Platform) MissingNoteImages(ctx context.Context, limit int) ([]MissingMedia, error) {
	return p.missingMedia(ctx, "иллюстрации без байтов", `
		SELECT note_id, url FROM note_images
		 WHERE sha256 IS NULL
		 ORDER BY note_id, position
		 LIMIT $1`, limit)
}

func (p *Platform) missingMedia(ctx context.Context, what, sql string, limit int) ([]MissingMedia, error) {
	rows, err := p.pool.Query(ctx, sql, limit)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	defer rows.Close()
	var out []MissingMedia
	for rows.Next() {
		var m MissingMedia
		if err := rows.Scan(&m.ID, &m.URL); err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// mainImageFirst — порядок иллюстраций заметки, ГЛАВНАЯ первой.
//
// Понадобился он потому, что у одной заметки картинок бывает НЕСКОЛЬКО, а
// показать в ленте, в канале и наверху страницы можно ровно одну. Копятся они у
// ЗЕРКАЛЬНОЙ заметки: автор меняет иллюстрацию на НГС, а мы видим не замену, а
// новый адрес — ключ у строки её ссылка (0003_note_images_url.sql), поэтому
// прежняя остаётся лежать. У заметки 313234 так собралось четыре штуки: наша,
// неверная с НГС, исправленная там же и снятая с нашей страницы.
//
// Правило владельца (10.09.2026): главная — НАША, если она есть, иначе
// ПОСЛЕДНЯЯ с НГС. Своё поставил администратор осознанно; среди чужих верна
// свежая — прежнюю потому и заменили. «Наше» отличается ПУСТЫМ
// media.source_url, а не видом адреса (см. шапку файла): у картинки,
// принесённой участником, источника нет вовсе, и по этой пустоте своё
// отделяется от привезённого в обеих ветках SetNoteImageAsAdmin.
//
// Первым ключом идут БАЙТЫ: строка, у которой известна одна ссылка, не
// нарисуется вовсе (MediaURL у неё пуст), и главной ей быть нельзя. Строку
// media она при этом не находит, source_url читается пустым — то есть без этого
// ключа не забранная картинка НГС притворилась бы нашей.
//
// Порядок ОДИН на все три места намеренно: разойдись они, читатель увидел бы в
// ленте одну картинку, на странице другую, а в канале третью. Алиасы в нём
// зашиты (i — note_images, mi — media), поэтому все три запроса зовут таблицы
// одинаково.
const mainImageFirst = `(i.sha256 IS NOT NULL) DESC, (coalesce(mi.source_url, '') = '') DESC, i.position DESC`

// NoteThumbs — ГЛАВНАЯ иллюстрация каждой из названных заметок (mainImageFirst).
//
// Отдельный метод, а не NoteImages в цикле: лента показывает двадцать заметок, и
// двадцать запросов вместо одного — это ровно тот расход, из-за которого лента
// когда-то и получила свой индекс. Одна, а не все: в ленте карточка одна, а
// галерея живёт на странице заметки.
//
// Строки без байтов (sha256 IS NULL — знаем ссылку, файла ещё нет) пропускаются:
// показывать в ленте нечего, а гонять читателя на hsmedia.ru мы не станем.
func (p *Platform) NoteThumbs(ctx context.Context, ids []int64) (map[int64]Media, error) {
	out := make(map[int64]Media, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := p.pool.Query(ctx, `
		SELECT DISTINCT ON (i.note_id)
		       i.note_id, i.sha256, coalesce(mi.mime, ''),
		       coalesce(mi.width, 0), coalesce(mi.height, 0)
		  FROM note_images i
		  JOIN media mi ON mi.sha256 = i.sha256
		 WHERE i.note_id = ANY($1)
		 ORDER BY i.note_id, `+mainImageFirst, ids)
	if err != nil {
		return nil, fmt.Errorf("иллюстрации ленты: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var m Media
		if err := rows.Scan(&id, &m.SHA256, &m.MIME, &m.Width, &m.Height); err != nil {
			return nil, fmt.Errorf("иллюстрации ленты: %w", err)
		}
		m.URL = MediaURL(m.SHA256, m.MIME)
		if m.URL != "" {
			out[id] = m
		}
	}
	return out, rows.Err()
}

// NoteImageCounts — сколько иллюстраций привязано к каждой заметке. Нужен
// сверке: картинку автор может дописать к уже опубликованной заметке (сайт
// отправляет её на премодерацию и возвращает), и заметить это иначе нечем.
func (p *Platform) NoteImageCounts(ctx context.Context) (map[int64]int, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT note_id, count(*) FROM note_images GROUP BY note_id`)
	if err != nil {
		return nil, fmt.Errorf("счётчики иллюстраций: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]int)
	for rows.Next() {
		var (
			id int64
			n  int
		)
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("счётчики иллюстраций: %w", err)
		}
		out[id] = n
	}
	return out, rows.Err()
}

// NoteImages — иллюстрации заметки в порядке показа, ГЛАВНАЯ первой
// (mainImageFirst). URL наш; у не забранных байтов он пуст, и шаблон такую
// картинку просто не рисует.
//
// SourceURL здесь — источник ФАЙЛА (media.source_url), а не адрес строки
// note_images: по его пустоте и отличается наша картинка от привезённой, и
// спрашивают её ровно за этим. Прежде сюда попадала i.url, то есть у зеркальной
// строки чужой адрес на hsmedia.ru; читателей у поля не было ни одного, а смысл
// его расходился с тем, что кладёт Put.
func (p *Platform) NoteImages(ctx context.Context, noteID int64) ([]Media, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT i.sha256, coalesce(mi.mime, ''), coalesce(mi.bytes, 0),
		       coalesce(mi.width, 0), coalesce(mi.height, 0), coalesce(mi.source_url, '')
		  FROM note_images i
		  LEFT JOIN media mi ON mi.sha256 = i.sha256
		 WHERE i.note_id = $1
		 ORDER BY `+mainImageFirst, noteID)
	if err != nil {
		return nil, fmt.Errorf("иллюстрации заметки %d: %w", noteID, err)
	}
	defer rows.Close()

	var out []Media
	for rows.Next() {
		var m Media
		if err := rows.Scan(&m.SHA256, &m.MIME, &m.Bytes, &m.Width, &m.Height, &m.SourceURL); err != nil {
			return nil, fmt.Errorf("иллюстрации заметки %d: %w", noteID, err)
		}
		m.URL = MediaURL(m.SHA256, m.MIME)
		out = append(out, m)
	}
	return out, rows.Err()
}

// MediaURL — адрес файла для страницы. Пусто, если байтов у нас нет: пустая
// ссылка честнее подстановки чужого адреса, и она же держит правило «ни одна
// страница не ходит на hsmedia.ru».
func MediaURL(sha []byte, mime string) string {
	if len(sha) == 0 || mime == "" {
		return ""
	}
	h := hex.EncodeToString(sha)
	return MediaURLPrefix + h[:2] + "/" + h + mediaExt(mime)
}

// mediaExt — расширение по типу. Нужно не украшения ради: файлы отдаёт Caddy, а
// он определяет Content-Type по расширению, и без него картинка уедет
// октет-потоком.
func mediaExt(mime string) string {
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".bin"
	}
}

// detectMIME определяет тип по содержимому, а не по расширению ссылки: на НГС
// «.jpg» регулярно оказывается png, а геоблок — вообще html.
func detectMIME(data []byte) string {
	mime := http.DetectContentType(data)
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	return mime
}

func nullDim(v int) *int {
	if v <= 0 {
		return nil
	}
	return &v
}
