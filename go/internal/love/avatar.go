package love

import "strings"

// IsRealAvatar отличает загруженное фото от дефолтного силуэта: плейсхолдеры
// лежат в /static/ на самом сайте (male300px.png, female300px.png,
// anonymous300px.png), настоящие фото — на CDN абсолютной ссылкой
// (hsmedia.ru/cache/love/avatars/… и /preview/love/avatars/…).
//
// Правило про сайт, поэтому живёт здесь, а не у потребителей: их двое —
// зеркало (силуэт в мессенджер не тащим) и площадка (силуэт не кладём в
// хранилище: «аватар есть у всех» — это не аватар, а фон).
func IsRealAvatar(url string) bool {
	return strings.HasPrefix(url, "http") && !strings.Contains(url, "/static/")
}
