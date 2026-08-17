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
	return &MediaStore{p: p, dir: dir}, nil
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
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		m.Width, m.Height = cfg.Width, cfg.Height
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

// Has — файл уже в хранилище (проверяется диск, а не база: правда — на диске).
func (s *MediaStore) Has(sha []byte, mime string) bool {
	if len(sha) == 0 {
		return false
	}
	_, err := os.Stat(s.FilePath(sha, mime))
	return err == nil
}

// AttachNoteImage привязывает иллюстрацию к заметке. sha пуст — URL знаем, а
// байты ещё не забрали (заберём позже, ссылка на hsmedia.ru наружу не уходит).
func (p *Platform) AttachNoteImage(ctx context.Context, noteID int64, position int, sha []byte, url string) error {
	var shaArg any
	if len(sha) > 0 {
		shaArg = sha
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO note_images (note_id, position, sha256, url)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (note_id, position) DO UPDATE
		   SET sha256 = coalesce(excluded.sha256, note_images.sha256),
		       url    = excluded.url`,
		noteID, position, shaArg, url)
	return wrapf(err, "иллюстрация %d заметки %d", position, noteID)
}

// NoteImages — иллюстрации заметки в порядке показа. URL наш; у не забранных
// байтов он пуст, и шаблон такую картинку просто не рисует.
func (p *Platform) NoteImages(ctx context.Context, noteID int64) ([]Media, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT i.sha256, coalesce(m.mime, ''), coalesce(m.bytes, 0),
		       coalesce(m.width, 0), coalesce(m.height, 0), i.url
		  FROM note_images i
		  LEFT JOIN media m ON m.sha256 = i.sha256
		 WHERE i.note_id = $1
		 ORDER BY i.position`, noteID)
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
