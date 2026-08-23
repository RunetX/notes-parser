package web

// Бумаги площадки: соглашения, политика конфиденциальности и отказ от
// ответственности. Три страницы, один шаблон.
//
// Стоят они в подвале КАЖДОЙ страницы, и это не украшение подвала. Политику
// обработки закон велит опубликовать так, чтобы её мог прочесть любой и не
// спрашивая разрешения (ч. 2 ст. 18.1), — значит она обязана быть открыта
// гостю, а не лежать за входом на «Моей странице». Отказ от ответственности
// нужен тому же гостю: он читает чужие тексты, и знать, чьи они и с кого
// спрашивать, ему нужно ДО того, как он на них наткнётся.
//
// Тексты — файлы, а не разметка в шаблоне: их пишет и правит человек, а не
// вёрстка, и правка не должна требовать знания HTML. Разбирает их та же
// docHTML, что показывает согласия на экране входа, — второго способа
// превратить наш текст в страницу не заводится.
//
// Из этого следует правило для того, кто станет их править: перенос строки в
// файле — это <br> на странице. Абзац пишется ОДНОЙ длинной строкой, и каждый
// пункт списка тоже; ручная вёрстка по 80 знаков, привычная в исходниках, на
// телефоне даст рваный текст с обрывом посреди фразы. Тексты согласий рядом
// свёрнуты по-старому, и переписать их нельзя: опубликованная редакция
// неизменяема, а правка ради красоты обесценила бы все данные согласия.
//
// Соглашения при этом НЕ дублируются: страница показывает те же самые тексты,
// что подписывают при входе (platform.CurrentConsentDocs), с теми же
// реквизитами оператора. Копия документа, живущая своей жизнью, — это ровно
// тот случай, когда через год расходятся оригинал и то, что видят люди.

import (
	"embed"
	"fmt"
	"net/http"
	"strings"
	texttpl "text/template"

	"lovegw/internal/platform"
)

//go:embed docs
var docFS embed.FS

type docPage struct {
	page
	// Lead — строка над документами. Есть только у страницы соглашений: их два,
	// и объяснить, откуда они взялись, нужно до, а не после.
	Lead string
	// Docs — тексты целиком. Заголовок документа — его первая строка, поэтому
	// своего h1 у страницы нет: печатать название дважды незачем.
	Docs []string
}

// handleConsentDocs — оба согласия одной страницей, открытой всем.
//
// Путь /consents (множественное) — это ЧТЕНИЕ документов, а /consent
// (единственное) — шаг входа, где их подписывают. Соседство неудачное, но
// адрес шага уже лежит в чужих историях браузера, а название страницы
// «Соглашения» иначе не назвать.
func (s *Server) handleConsentDocs(w http.ResponseWriter, r *http.Request) {
	docs, err := platform.CurrentConsentDocs(s.cfg.Operator)
	if err != nil {
		s.oops(w, r, "тексты согласий", err)
		return
	}
	bodies := make([]string, 0, len(docs))
	for _, d := range docs {
		bodies = append(bodies, d.Body)
	}
	s.render(w, r, http.StatusOK, "doc.gohtml", docPage{
		page: s.newPage(r, "Соглашения"),
		Lead: "Это те самые два документа, которые площадка показывает при входе. " +
			"Читать их можно и не входя: решать, соглашаться ли, проще до того, " +
			"как начал.",
		Docs: bodies,
	})
}

func (s *Server) handlePrivacy(w http.ResponseWriter, r *http.Request) {
	s.showDoc(w, r, "privacy.txt")
}

func (s *Server) handleDisclaimer(w http.ResponseWriter, r *http.Request) {
	s.showDoc(w, r, "disclaimer.txt")
}

// showDoc рисует один наш документ. Заголовок вкладки берётся из первой строки
// файла, а не пишется рядом в коде: разойдясь, они дали бы страницу, которая
// в браузере зовётся не так, как называет себя сама.
func (s *Server) showDoc(w http.ResponseWriter, r *http.Request, name string) {
	title, body, err := ownDoc(name, s.cfg.Operator)
	if err != nil {
		s.oops(w, r, "текст документа", err)
		return
	}
	s.render(w, r, http.StatusOK, "doc.gohtml", docPage{
		page: s.newPage(r, title),
		Docs: []string{body},
	})
}

// ownDoc — текст документа с подставленными реквизитами оператора.
//
// Подстановка та же, что у согласий, и по той же причине: имя и контакт
// оператора приезжают из конфига, а написанные в тексте словами превратили бы
// документ во второй источник правды о том, кто обрабатывает данные.
// missingkey=error — чтобы опечатка в имени поля стала отказом при показе, а не
// строкой «<no value>» посреди политики.
func ownDoc(name string, op platform.Operator) (title, body string, err error) {
	raw, err := docFS.ReadFile("docs/" + name)
	if err != nil {
		return "", "", fmt.Errorf("документ %s: %w", name, err)
	}
	// text/template, а не html: документ показывается как текст, экранирование
	// делает уже docHTML на показе.
	t, err := texttpl.New(name).Option("missingkey=error").Parse(string(raw))
	if err != nil {
		return "", "", fmt.Errorf("документ %s: %w", name, err)
	}
	var b strings.Builder
	if err := t.Execute(&b, op.Public()); err != nil {
		return "", "", fmt.Errorf("документ %s: %w", name, err)
	}
	body = b.String()
	head, _, _ := strings.Cut(body, "\n")
	return strings.TrimSpace(head), body, nil
}
