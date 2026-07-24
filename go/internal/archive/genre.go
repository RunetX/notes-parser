package archive

// Жанр эталона атрибуции — из какого текста собран профиль автора. Заметку
// атрибутируем эталоном того же регистра: профиль из комментариев (короткие
// реплики, часто в один смайл) — чужой жанр для заметки, отсюда системная
// ошибка сопоставления «запрос-заметка ↔ эталон-в-комментариях».
//
//	GenreAll   — комментарии + заметки: полный отпечаток (склейка альтов, диагностика).
//	GenreNotes — только заметки: register-matched эталон для атрибуции заметки.
const (
	GenreAll   = "all"
	GenreNotes = "notes"
)

// ValidGenre — известный ли жанр (валидация флага CLI).
func ValidGenre(g string) bool { return g == GenreAll || g == GenreNotes }

// genreSources — SQL-запросы (author_id, text), из которых собирается профиль
// жанра. Общие для стилометрии и лексики: отличается только извлекаемый признак
// (3-граммы vs слова), не источник текста. notes — только заметки; all —
// комментарии И заметки (полный авторский текст).
func genreSources(genre string) []string {
	notes := `SELECT author_id, text FROM notes WHERE author_id IS NOT NULL AND author_id != 0`
	if genre == GenreNotes {
		return []string{notes}
	}
	comments := `SELECT author_id, text FROM comments WHERE author_id != 0`
	return []string{comments, notes}
}
