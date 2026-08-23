package web

// Ролик на странице — КАРТОЧКА со своим превью, а не встроенный плеер.
//
// Решение владельца 23.08.2026: «добавлять полноценную систему работы с видео
// дорого, а ролики с YouTube — нет, площадка уже отмодерировала контент». Из
// трёх возможных видов (плеер сразу, плеер по клику, карточка) выбран самый
// скромный, и все три довода за него — не про вкус, а про уже принятые правила.
//
// ПЕРВЫЙ — согласие. Опубликованный текст обещает людям, что их данные «не
// вывозятся за пределы России», а <iframe> — это запрос БРАУЗЕРА ЧИТАТЕЛЯ к
// Google на каждый показ страницы: туда уезжает его адрес и то, какую заметку он
// в эту минуту читает, — и уезжает нашей рукой. Ровно этим доводом 23.08.2026
// автомат модерации увели с Claude на Yandex; новая редакция согласия обесценила
// бы все прежние (Has требует Version >= version).
//
// ВТОРОЙ — CSP. frame-src пробивает тот самый забор, которым держится «ни npm,
// ни CDN» (web.go, withSecurityHeaders). Карточка его не трогает вовсе: превью
// лежит у нас и приезжает с 'self'.
//
// ТРЕТИЙ — YouTube в России замедлен, и встроенный плеер у большинства читателей
// был бы чёрным прямоугольником. Наше превью отдаёт Caddy с того же диска, что и
// аватары.
//
// Клик при этом уводит на сам ролик, то есть остаётся ТЕМ ЖЕ переходом по чужому
// адресу, который человек делает и сегодня, — только теперь он заранее видит,
// что там ролик, и какой.

import (
	"net/url"
	"regexp"
	"strings"

	"lovegw/internal/platform"
	"lovegw/internal/textutil"
)

// videoCard — то, что рисует шаблон. Собирается целиком в Go: условие «ссылка
// опознана, превью лежит, текст наш» на языке шаблонов читается вдвое хуже.
type videoCard struct {
	Provider string // подпись: «YouTube», «Rutube»
	URL      string // адрес ролика — туда уходит клик
	Preview  string // наш адрес превью, всегда /media/…
	Text     string // укороченный адрес под превью
}

// videoLinkTextLimit — сколько адреса показывать в подписи карточки. Меньше, чем
// у обычной ссылки (linkTextLimit): под превью адрес стоит отдельной строкой, а
// не в потоке текста, и переносить его там нечем.
const videoLinkTextLimit = 48

// videoCards — ролики, на которые ссылается этот текст.
//
// Разворачивается ссылка ТОЛЬКО в написанном ЗДЕСЬ, и это тот же рубеж, что у
// разметки и смайлов (см. eraOf): своё разбираем целиком, чужое показываем так,
// как показывал его сайт, — а НГС ссылок в ролики не разворачивал никогда. Довод
// не только исторический: зеркальных реплик 10,7 млн за тринадцать лет, и превью
// к ним пришлось бы тянуть лениво, на первом показе, миллионами запросов к чужим
// хостам.
//
// Повторы схлопываются: одна и та же ссылка, названная в реплике дважды, — это
// один ролик, а не две карточки.
func videoCards(id int64, body string) []videoCard {
	var out []videoCard
	for _, r := range videoRefs(id, body) {
		// Превью нет — карточки нет, и ссылка остаётся ровно тем текстом, чем
		// была до этой правки. Это и есть страховка на все случаи, которых мы
		// отсюда не проверим: ролик снесли, чужой хост сменил адрес картинки,
		// сеть до него не доходит. Заодно has подталкивает закачку — к
		// следующему показу карточка появится сама.
		if !previews.has(r) {
			continue
		}
		out = append(out, videoCard{
			Provider: r.p.name,
			URL:      r.page,
			Preview:  r.previewURL(),
			Text:     textutil.Fit(r.page, videoLinkTextLimit),
		})
	}
	return out
}

// videoRefs — опознанные ролики этого текста, в порядке появления и без
// повторов. Отделено от videoCards, потому что спрашивают об этом двое и с
// разными намерениями: показ — «что нарисовать», публикация — «за чем сходить».
func videoRefs(id int64, body string) []videoRef {
	if previews == nil || !platform.IsNative(id) {
		return nil
	}
	var (
		out  []videoRef
		seen map[string]bool
	)
	for _, loc := range linkRe.FindAllStringIndex(body, -1) {
		addr := strings.TrimRight(body[loc[0]:loc[1]], linkTailCut)
		r, ok := parseVideoURL(addr)
		if !ok {
			continue
		}
		key := r.key()
		if seen[key] {
			continue
		}
		if seen == nil {
			seen = make(map[string]bool)
		}
		seen[key] = true
		out = append(out, r)
	}
	return out
}

// ---------------------------------------------------------------- опознание

// provider — площадка видео. Список ЗАКРЫТЫЙ и живёт здесь, а не в конфиге:
// «какие чужие хосты площадка показывает картинкой от своего имени» — это
// решение, которое обязано меняться правкой, видной в diff, а не настройкой.
//
// Rutube рядом с YouTube не для полноты списка: он не замедлен, и вопроса о
// вывозе данных не задаёт вовсе — на нём карточка работает лучше, чем на первом.
// Попасть в список — значит уметь отдать превью АНОНИМНО; кто не умеет, тот не
// площадка для нас (см. про VK ниже).
type provider struct {
	name string // подпись карточки
	dir  string // каталог превью внутри media/v
	// hosts — точные имена хостов. Сравнение по списку, а не по вхождению
	// подстроки: «youtube.com.evil.example» прошёл бы по буквам, и мы своей
	// рукой поставили бы чужую картинку на свою страницу. Тот же довод, что у
	// isOwnLink.
	hosts []string
	// videoID достаёт идентификатор из разобранного адреса. Пусто — не ролик.
	videoID func(u *url.URL) string
	// id — какой вид у идентификатора этой площадки. Проверяется обязательно:
	// id уходит в ИМЯ ФАЙЛА, и «..» в нём стоил бы записи мимо каталога.
	id *regexp.Regexp
	// thumb — прямой адрес превью, если он выводится из id. Пусто — спросить
	// oEmbed (см. oembed).
	thumb func(id string) string
	// oembed — адрес oEmbed-описания страницы; из ответа берётся thumbnail_url.
	oembed func(pageURL string) string
}

var providers = []provider{{
	name: "YouTube",
	dir:  "yt",
	hosts: []string{
		"youtube.com", "www.youtube.com", "m.youtube.com",
		"youtu.be", "www.youtu.be",
		"youtube-nocookie.com", "www.youtube-nocookie.com",
	},
	// Идентификатор — ровно 11 знаков из алфавита base64url.
	id: regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`),
	videoID: func(u *url.URL) string {
		path := strings.Trim(u.Path, "/")
		switch {
		case strings.HasSuffix(strings.ToLower(u.Hostname()), "youtu.be"):
			return firstSegment(path) // youtu.be/<id>
		case path == "watch":
			return u.Query().Get("v") // youtube.com/watch?v=<id>
		case strings.HasPrefix(path, "shorts/"), strings.HasPrefix(path, "embed/"),
			strings.HasPrefix(path, "live/"), strings.HasPrefix(path, "v/"):
			return firstSegment(path[strings.IndexByte(path, '/')+1:])
		}
		return ""
	},
	// Прямой адрес, без обращения к чужому API: hqdefault есть у всякого живого
	// ролика, а у снесённого его нет — то есть отсутствие превью само означает
	// «показывать нечего», и лишнего запроса на это не тратится.
	thumb: func(id string) string { return "https://i.ytimg.com/vi/" + id + "/hqdefault.jpg" },
}, {
	name:  "Rutube",
	dir:   "rt",
	hosts: []string{"rutube.ru", "www.rutube.ru"},
	id:    regexp.MustCompile(`^[0-9a-f]{32}$`),
	videoID: func(u *url.URL) string {
		path := strings.Trim(u.Path, "/")
		if !strings.HasPrefix(path, "video/") {
			return ""
		}
		return firstSegment(strings.TrimPrefix(path, "video/"))
	},
	oembed: func(page string) string {
		return "https://rutube.ru/api/oembed/?format=json&url=" + url.QueryEscape(page)
	},
}}

// VK ВИДЕО В СПИСКЕ НЕТ, и это замер, а не забывчивость.
//
// Ссылок на него в наших комментариях больше, чем на все остальные площадки
// вместе (замер 23.08.2026 по свежей части таблицы: 10 адресов `m.vk.com/video`
// из 12 найденных), поэтому соблазн добавить его будет возникать снова. Взять
// превью анонимно нечем: `vk.com/oembed` отдаёт HTML-404, а страница ролика
// уводит редиректом на `login.vk.ru`. Осталось бы ходить под токеном VK API —
// то есть завести ещё один секрет и ещё одну зависимость ради картинки.
//
// Пока этого нет, ссылка на VK остаётся текстом, как была. Держать площадку в
// списке «на будущее» нельзя: каждая такая ссылка заводила бы фоновую закачку,
// которая ГАРАНТИРОВАННО не удаётся, — и повторяла бы попытку раз в шесть часов
// до конца времён.

// videoRef — опознанный ролик.
type videoRef struct {
	p    *provider
	id   string
	page string
}

// key — он же путь превью без расширения: «yt/dQw4w9WgXcQ».
func (r videoRef) key() string { return r.p.dir + "/" + r.id }

func (r videoRef) previewURL() string {
	return platform.MediaURLPrefix + previewDir + "/" + r.key() + previewExt
}

// parseVideoURL опознаёт ссылку на ролик. Проверяются и хост (по точному
// списку), и форма идентификатора — второе не придирка: id уходит в имя файла.
func parseVideoURL(addr string) (videoRef, bool) {
	u, err := url.Parse(addr)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return videoRef{}, false
	}
	host := strings.ToLower(u.Hostname())
	for i := range providers {
		p := &providers[i]
		if !hasHost(p.hosts, host) {
			continue
		}
		id := p.videoID(u)
		if id == "" || !p.id.MatchString(id) {
			return videoRef{}, false
		}
		return videoRef{p: p, id: id, page: addr}, true
	}
	return videoRef{}, false
}

func hasHost(list []string, host string) bool {
	for _, h := range list {
		if h == host {
			return true
		}
	}
	return false
}

func firstSegment(path string) string {
	if i := strings.IndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return path
}
