package web

// ИМЕНИ ПРЕЖНЕГО САЙТА НА СТРАНИЦАХ НЕТ (решение владельца 12.09.2026).
//
// Площадка стои́т на текстах и людях, переехавших с чужого сайта, и первые
// недели говорила об этом прямым именем в сотне мест: «на НГС», «анкета НГС»,
// «love.ngs.ru». Решено убрать имя целиком — Зазеркалье не филиал и не
// продолжение, а «прежний сайт» говорит человеку ровно столько, сколько ему
// нужно, чтобы понять инструкцию.
//
// Механику это не трогает: вход по коду в анкете, отправка записей и аватар
// работают как работали, — а раз механика осталась, то и соблазн вернуть имя
// «для ясности» останется навсегда. Отсюда СТОРОЖ, и он двойной.
//
// Первый — по ШАБЛОНАМ, и он важнее: страницу можно забыть отрендерить в тесте,
// а забыть шаблон нельзя, их читает go:embed целиком. Комментарии из проверки
// вырезаются: там имя стои́т законно и нужно тому, кто будет это править
// (историю решения нельзя переписывать вслед за текстом — иначе через месяц
// никто не вспомнит, откуда взялся «прежний сайт»).
//
// Второй — по ГОТОВЫМ СТРАНИЦАМ: часть слов приезжает не из шаблона, а из Go
// (метки происхождения, подписи тем справки, отказы формы входа), и первым
// сторожем они не закрыты вовсе.
//
// ЧЕГО СТОРОЖ НЕ КАСАЕТСЯ — БУМАГИ, и это не забывчивость. «Отказ от
// ответственности» защищает владельца ровно тем, что называет сайт и отрицает
// связь с его редакцией; «Политика» обязана называть источник данных и то, куда
// они уходят (ч. 3 ст. 18 152-ФЗ), а обезличенная формулировка сделала бы её
// неправдой. Пять опубликованных согласий неизменяемы по построению: правка при
// том же номере версии — намеренный отказ `platform migrate`, а новая редакция
// означает переподписку всеми до единого.

import (
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"
)

// oldSiteName — имя, которого на страницах быть не должно, во всех написаниях.
var oldSiteName = regexp.MustCompile(`НГС|ngs\.ru`)

// tplComment — комментарий шаблона: имя внутри него законно.
var tplComment = regexp.MustCompile(`(?s)\{\{/\*.*?\*/\}\}`)

func TestИмениПрежнегоСайтаНетВШаблонах(t *testing.T) {
	err := fs.WalkDir(templateFS, "templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := fs.ReadFile(templateFS, p)
		if err != nil {
			return err
		}
		body := tplComment.ReplaceAllString(string(raw), "")
		for i, line := range strings.Split(body, "\n") {
			if oldSiteName.MatchString(line) {
				t.Errorf("%s:%d — имя прежнего сайта в видимом тексте: %s",
					path.Base(p), i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// А это второй сторож: слова, приезжающие из Go. Страницы взяты те, где текста
// больше всего и где имя стояло чаще всего, — справка целиком и обе двери входа.
func TestИмениПрежнегоСайтаНетНаСтраницах(t *testing.T) {
	cfg := Config{Contacts: Contacts{
		ProfileID:   1493279,
		Telegram:    "https://t.me/zazerkalje",
		BotTelegram: "https://t.me/bot",
		BotMAX:      "https://max.ru/bot",
	}, Support: Support{URL: "https://example.org/donate", InfraRub: 1, ModelsRub: 1, AsOf: "12.09.2026"}}
	pages := []string{"/help", "/login", "/login/invite"}
	for _, topic := range helpTopics {
		pages = append(pages, "/help/"+topic.Slug)
	}
	for _, p := range pages {
		body := helpBody(t, p, cfg)
		if m := oldSiteName.FindString(body); m != "" {
			i := strings.Index(body, m)
			t.Errorf("%s: имя прежнего сайта на готовой странице (…%s…)",
				p, strings.TrimSpace(body[max(0, i-90):min(len(body), i+60)]))
		}
	}
}

// А метки происхождения — тот же вопрос, но у них своя дверь: они собираются в
// Go и на страницу попадают заголовком и словом для читалки.
func TestМеткиПроисхожденияНеНазываютПрежнийСайт(t *testing.T) {
	for _, o := range []noteOrigin{
		originOf(312811, false, false),
		originOf(100000000001, false, true),
		commentOriginOf(63238683, false),
		commentOriginOf(100000000001, false),
		commentOriginOf(100000000002, true),
	} {
		if oldSiteName.MatchString(o.Label) || oldSiteName.MatchString(o.Title) {
			t.Errorf("метка %q называет прежний сайт по имени: %s", o.Label, o.Title)
		}
	}
}
