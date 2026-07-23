// Пакет love — клиент сайта love.ngs.ru: скрапинг ленты заметок и
// комментариев, логин, отправка комментариев и заметок от имени пользователя.
package love

import (
	"fmt"
	"time"
)

// Note — заметка из ленты /notes/.
type Note struct {
	ID              string    `json:"id"`
	AuthorID        string    `json:"author_id"` // "0" — аноним
	AuthorName      string    `json:"author_name"`
	Text            string    `json:"text"`
	AuthorAvatarURL string    `json:"author_avatar_url"`      // src аватара автора (может быть плейсхолдером)
	Images          []string  `json:"images"`                 // URL иллюстраций заметки
	CommentsClosed  bool      `json:"comments_closed"`        // сайт пометил заметку «не актуальна»: новые комментарии закрыты
	PublishedAt     time.Time `json:"published_at,omitempty"` // дата заметки; заполняется со страницы комментариев, из ленты — нулевая
}

// Comment — комментарий к заметке. Страница отдаёт их от новых к старым
// (desc); порядок среза соответствует порядку в документе.
type Comment struct {
	ID          int64     `json:"id"`
	ParentID    int64     `json:"parent_id"` // id родительского комментария; 0 — корень заметки. Заполняется только в древовидном виде (?view=tree)
	AuthorID    string    `json:"author_id"` // числовой id анкеты автора (из ссылки профиля); связывает авторов заметок и комментариев
	AuthorName  string    `json:"author_name"`
	AuthorAge   string    `json:"author_age"`
	AuthorLink  string    `json:"author_link"` // абсолютный URL анкеты
	AvatarURL   string    `json:"avatar_url"`  // абсолютный URL аватара
	PublishedAt time.Time `json:"published_at"`
	Text        string    `json:"text"`
}

// MarkupError — обязательный селектор не дал результата или значение
// не разобралось: признак дрейфа вёрстки сайта, а не пустой страницы.
type MarkupError struct {
	Selector string // селектор или поле, которое не нашлось
	Context  string // где именно (лента, комментарий N, ...)
}

func (e *MarkupError) Error() string {
	return fmt.Sprintf("вёрстка изменилась? не разобран %q (%s)", e.Selector, e.Context)
}
