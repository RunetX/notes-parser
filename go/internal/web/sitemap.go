package web

// Карта сайта — единственная страница площадки, написанная НЕ для человека.
//
// Появилась она вместе со снятием запрета индексации (23.08.2026): своим ходом
// робот дошёл бы до заметок только через постраничку ленты, а это пять с
// лишним тысяч страниц по двадцать записей, и обходят такое поисковики неохотно.
//
// Устроена она в два уровня, как велит формат: /sitemap.xml — оглавление, а
// адреса лежат в /sitemap/1.xml, /2.xml и так далее по 50 000. Отдаётся всё
// потоком, без сборки в буфер: пятьдесят тысяч адресов это мегабайты, и держать
// их в памяти ради одного робота незачем — тем более что рядом на том же ядре
// живёт зеркало.
//
// Скрытое сюда не попадает вовсе (запрос отбирает status = 0): карта обязана
// называть ровно то, что робот увидит, иначе он сам себе объяснит расхождение —
// «страница отдаёт 404, значит сайт врёт» — и станет ходить реже.

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lovegw/internal/platform"
)

// sitemapPages — сколько файлов нужно на такое число заметок. Ноль заметок —
// один пустой файл, а не ноль: оглавление без единой ссылки робот считает
// поломкой.
func sitemapPages(total int) int {
	n := (total + platformSitemapLimit - 1) / platformSitemapLimit
	if n < 1 {
		n = 1
	}
	return n
}

// handleSitemapIndex — оглавление карты.
func (s *Server) handleSitemapIndex(w http.ResponseWriter, r *http.Request) {
	total, err := s.st.CountNotes(r.Context(), platform.Viewer{})
	if err != nil {
		s.oops(w, r, "карта сайта", err)
		return
	}
	base := strings.TrimRight(s.cfg.BaseURL, "/")
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	fmt.Fprint(w, xmlHead+`<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`+"\n")
	for i := 1; i <= sitemapPages(total); i++ {
		fmt.Fprintf(w, "<sitemap><loc>%s/sitemap/%d.xml</loc></sitemap>\n", base, i)
	}
	fmt.Fprint(w, "</sitemapindex>\n")
}

// handleSitemapPage — одна порция адресов.
func (s *Server) handleSitemapPage(w http.ResponseWriter, r *http.Request) {
	// Имя файла берётся сегментом целиком: шаблон роутера обязан занимать
	// сегмент полностью («/sitemap-{num}.xml» он не принимает вовсе), поэтому
	// «.xml» снимаем сами — и требуем, иначе адрес карты стал бы двумя разными.
	name := r.PathValue("name")
	num, err := strconv.Atoi(strings.TrimSuffix(name, ".xml"))
	if err != nil || !strings.HasSuffix(name, ".xml") || num < 1 || num > 10000 {
		s.fail(w, r, http.StatusNotFound, "Такой страницы нет.")
		return
	}
	notes, err := s.st.SitemapNotes(r.Context(), (num-1)*platformSitemapLimit, platformSitemapLimit)
	if err != nil {
		s.oops(w, r, "карта сайта", err)
		return
	}
	base := strings.TrimRight(s.cfg.BaseURL, "/")
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	fmt.Fprint(w, xmlHead+`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`+"\n")
	// Первая страница карты начинается с самой площадки и бумаг: они меняются
	// редко, но найтись обязаны, а из ленты на них ведёт только шапка с подвалом.
	if num == 1 {
		for _, p := range []string{"/", "/help", "/consents", "/privacy", "/disclaimer"} {
			fmt.Fprintf(w, "<url><loc>%s%s</loc></url>\n", base, p)
		}
	}
	for _, n := range notes {
		fmt.Fprintf(w, "<url><loc>%s/n/%d</loc><lastmod>%s</lastmod></url>\n",
			base, n.ID, n.Changed.UTC().Format(time.RFC3339))
	}
	fmt.Fprint(w, "</urlset>\n")
}

const xmlHead = `<?xml version="1.0" encoding="UTF-8"?>` + "\n"

// Потолок формата живёт в ядре — там же, где запрос, который его соблюдает.
const platformSitemapLimit = platform.SitemapLimit
