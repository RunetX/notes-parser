package web

// Силуэт на месте пустого фото.
//
// Проверяется здесь не картинка, а РАЗВИЛКА, у которой три ветки с разной ценой
// ошибки: своё фото (показать вместо него чужое — хуже некуда), пол (назвать
// мужчиной женщину) и аноним (проступивший сквозь силуэт автор анонимки).

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"lovegw/internal/platform"
)

var avaRe = regexp.MustCompile(`<img class="ava([^"]*)" src="([^"]+)"`)

// avaSrcs — адреса всех аватаров страницы, в порядке показа.
func avaSrcs(body string) []string {
	var out []string
	for _, m := range avaRe.FindAllStringSubmatch(body, -1) {
		out = append(out, m[2])
	}
	return out
}

// Пол выбирает силуэт, как на НГС, а неизвестный пол получает НЕЙТРАЛЬНЫЙ, а не
// мужской: пол у нас приезжает обходом дерева и входом, поэтому у теней его
// сплошь и рядом нет — мужской «по умолчанию» назвал бы мужчинами половину
// площадки.
func TestSilhouetteChosenByGender(t *testing.T) {
	cases := []struct {
		name      string
		gender    platform.Gender
		anonymous bool
		want      string
	}{
		{"мужчина", platform.GenderMale, false, "profile/male300px."},
		{"женщина", platform.GenderFemale, false, "profile/female300px."},
		{"пол неизвестен", platform.GenderUnknown, false, "profile/unknown."},
		{"аноним", platform.GenderUnknown, true, "profile/anonymous300px."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := newAvatar("", c.gender, c.anonymous)
			if !strings.Contains(got.URL, c.want) {
				t.Errorf("силуэт %q, ожидался %q", got.URL, c.want)
			}
			if !got.Sil {
				t.Error("силуэт не помечен силуэтом — в тёмной теме он засветит страницу")
			}
		})
	}
}

// Своё фото сильнее силуэта — иначе площадка забыла бы то единственное, что
// зеркало успело принести про человека до смерти НГС.
func TestOwnPhotoWinsOverSilhouette(t *testing.T) {
	got := newAvatar("/media/ab/abcd.jpg", platform.GenderFemale, false)
	if got.URL != "/media/ab/abcd.jpg" {
		t.Errorf("вместо фото показано %q", got.URL)
	}
	if got.Sil {
		t.Error("фото помечено силуэтом — тёмная тема его пригасит")
	}
}

// А вот у анонима фото не показывается НИКОГДА, даже если ссылка каким-то
// образом доехала: под анонимной публикацией не должно проступать ничего о её
// авторе. Ядро его и не отдаёт (в NoteView нет такого поля), но граница
// держится в двух местах, а не в одном.
func TestAnonymousNeverShowsAPhoto(t *testing.T) {
	got := newAvatar("/media/ab/abcd.jpg", platform.GenderFemale, true)
	if !strings.Contains(got.URL, "profile/anonymous300px.") {
		t.Errorf("у анонима показано %q", got.URL)
	}
}

// Страница ленты и страница заметки берут силуэт оттуда же: аноним — «в шляпе и
// очках», подписанный автор без фото — по полу, автор с фото — своё.
func TestPagesShowSilhouettes(t *testing.T) {
	anon := platform.NoteView{ID: 312811, Anonymous: true, Body: "без подписи"}
	her := sampleNote()
	her.ID = 312812
	her.Author = platform.Author{ID: 1409563, Nick: "Клубника со льдом", Gender: platform.GenderFemale}

	st := &fakeStore{total: 3, notes: []platform.NoteView{anon, her, sampleNote()}}
	body := do(openServer(t, st), guest(t, "GET", "/")).Body.String()

	got := avaSrcs(body)
	if len(got) != 3 {
		t.Fatalf("аватаров на странице %d, ожидалось 3: %v", len(got), got)
	}
	for i, want := range []string{"profile/anonymous300px.", "profile/female300px.", "/media/ab/abcd.jpg"} {
		if !strings.Contains(got[i], want) {
			t.Errorf("аватар %d: %q, ожидался %q", i+1, got[i], want)
		}
	}

	// В треде то же самое: у безанкетного комментатора зеркала пола нет вовсе.
	st = &fakeStore{note: sampleNote(), thread: sampleThread()}
	body = do(openServer(t, st), guest(t, "GET", "/n/312811")).Body.String()
	for _, s := range avaSrcs(body)[1:] {
		if !strings.Contains(s, "profile/unknown.") {
			t.Errorf("в треде показан %q, ожидался нейтральный силуэт", s)
		}
	}
}

// Буквы ника на месте фото больше нет — ни на странице, ни в стилях. Она была
// нашей выдумкой: на НГС буквы нет нигде, там стоит силуэт.
func TestLetterAvatarIsGone(t *testing.T) {
	st := &fakeStore{total: 1, notes: []platform.NoteView{{ID: 312811, Anonymous: true, Body: "без подписи"}}}
	body := do(openServer(t, st), guest(t, "GET", "/")).Body.String()
	if strings.Contains(body, "ava none") {
		t.Error("на месте фото всё ещё буква")
	}
	if strings.Contains(cssText(t), ".ava.none") {
		t.Error("в стилях остались правила буквы")
	}
}

// Файлы вшиты в бинарник и отдаются картинкой, а не потоком байтов: PNG в
// assetMIME однажды не было, и силуэт приехал бы как application/octet-stream.
func TestSilhouetteFilesAreServedAsImages(t *testing.T) {
	for _, name := range []string{
		"profile/male300px.png", "profile/female300px.png",
		"profile/anonymous300px.png", "profile/unknown.png",
	} {
		url := assetURL(name)
		if url == "" {
			t.Errorf("%s не вшит в бинарник", name)
			continue
		}
		a, ok := assets[strings.TrimPrefix(url, "/assets/")]
		if !ok {
			t.Errorf("%s не отдаётся по адресу %s", name, url)
			continue
		}
		if a.mime != "image/png" {
			t.Errorf("%s отдаётся как %s", name, a.mime)
		}
		if len(a.data) == 0 {
			t.Errorf("%s пуст", name)
		}
	}
}

// [Ф] Имена трёх файлов сняты с записанных страниц сайта, а не выдуманы:
// anonymous300px стоит под анонимной заметкой в ленте, male300px/female300px —
// у комментаторов без фото. Четвёртого (unknown) на НГС нет и быть не может:
// там пол известен всегда.
func TestSilhouetteNamesComeFromTheSite(t *testing.T) {
	pages := map[string][]string{
		"../love/testdata/notes_feed.html":      {"anonymous300px.png"},
		"../love/testdata/comments_312696.html": {"male300px.png", "female300px.png"},
	}
	for path, names := range pages {
		raw, err := os.ReadFile(path)
		if err != nil {
			// Записанные страницы — чужое testdata; переехали, значит источник
			// придётся назвать заново, а не молча остаться без проверки.
			t.Skipf("записанная страница %s не читается: %v", path, err)
		}
		for _, n := range names {
			if !strings.Contains(string(raw), "/static/i/new/profile/"+n) {
				t.Errorf("в %s нет силуэта %s — имя разошлось с сайтом", path, n)
			}
		}
	}
}

// В тёмных палитрах силуэт пригашен, в светлой — нет. Тёмной темы на НГС не
// было вовсе, так что это не перенос, а наша правка поверх чужой картинки: фото
// есть далеко не у всех, и без затухания страница в темноте становится решёткой
// светлых квадратов ярче собственного текста. Настоящее фото не гасится никогда
// — оно содержание, а не фон.
func TestSilhouetteFadesOnlyInDarkPalettes(t *testing.T) {
	css := cssText(t)
	if !strings.Contains(css, ".ava.sil { opacity: var(--sil-fade, 1); }") {
		t.Fatal("силуэт не пригашается вовсе")
	}
	if strings.Contains(cssBlock(t, css, ":root {"), "--sil-fade") {
		t.Error("светлая палитра гасит силуэт — а на НГС он показан как есть")
	}
	for _, sel := range []string{`:root[data-theme="dark"] {`, `:root[data-theme="graphite"] {`} {
		if !strings.Contains(cssBlock(t, css, sel), "--sil-fade") {
			t.Errorf("тёмная палитра %s не гасит силуэт", sel)
		}
	}
}

// cssBlock — тело правила от селектора до ближайшей закрывающей скобки.
func cssBlock(t *testing.T, css, selector string) string {
	t.Helper()
	i := strings.Index(css, selector)
	if i < 0 {
		t.Fatalf("в стилях нет %s", selector)
	}
	rest := css[i+len(selector):]
	return rest[:strings.Index(rest, "}")]
}
