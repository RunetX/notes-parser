package platform

// Карта сайта: чем робот берёт архив.
//
// Понадобилась она в тот день, когда с площадки сняли запрет индексации
// (23.08.2026). Своим ходом робот нашёл бы заметки только через постраничку
// ленты — пять с лишним тысяч страниц по двадцать записей, — а глубокую
// постраничку поисковики обходят неохотно и не всю. Карта отдаёт те же адреса
// списком, и обход становится линейным.
//
// Отдаётся она ПОРЦИЯМИ по 50 000 (потолок формата), а порядок — по id: он же
// хронологический внутри полосы, и заметка не переезжает из порции в порцию от
// того, что в соседней кто-то ответил.

import (
	"context"
	"fmt"
	"time"
)

// SitemapLimit — сколько адресов в одном файле карты. Потолок формата — 50 000
// адресов либо 50 МБ; у нас строка адреса короткая, поэтому упираемся в первое.
const SitemapLimit = 50000

// SitemapNote — заметка глазами карты сайта: адрес и когда там что-то менялось.
type SitemapNote struct {
	ID      int64
	Changed time.Time
}

// sitemapQuery — заметки для карты. Скрытые (автором, модератором,
// обезличенные) не идут: карта обязана называть ровно то, что робот увидит.
//
// «Менялось» — это последний КОММЕНТАРИЙ, а не только публикация: тред живёт
// дольше заметки, и робот, судящий по дате публикации, второй раз к живому
// разговору не придёт.
//
// Плана этого запроса нет в договоре (pg_test): его спрашивают раз в сутки на
// весь архив, и перебор 117 тысяч строк с сортировкой по ключу здесь честнее,
// чем индекс, который никому больше не нужен.
const sitemapQuery = `
	SELECT id, greatest(published_at, coalesce(last_comment_at, published_at))
	  FROM notes WHERE status = 0
	 ORDER BY id
	 LIMIT $1 OFFSET $2`

// SitemapNotes — порция адресов для карты сайта.
func (p *Platform) SitemapNotes(ctx context.Context, offset, limit int) ([]SitemapNote, error) {
	if limit <= 0 || limit > SitemapLimit {
		limit = SitemapLimit
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := p.pool.Query(ctx, sitemapQuery, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("карта сайта: %w", err)
	}
	defer rows.Close()
	out := make([]SitemapNote, 0, 1024)
	for rows.Next() {
		var n SitemapNote
		if err := rows.Scan(&n.ID, &n.Changed); err != nil {
			return nil, fmt.Errorf("карта сайта: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
