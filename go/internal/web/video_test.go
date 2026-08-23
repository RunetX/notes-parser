package web

// Что стережёт этот файл: карточка ролика ставит на нашу страницу картинку по
// чужой ссылке, и цена ошибки здесь не косметическая. Поэтому проверяется не
// «красиво разобрали адрес», а три границы — чей хост, какой идентификатор и
// чей текст.

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lovegw/internal/platform"
)

// nativeID — идентификатор из нативной полосы: только в написанном ЗДЕСЬ
// ссылка вообще разворачивается в карточку.
const nativeID = platform.NativeIDBase + 17

// offlineTransport — сеть в тестах недоступна. Нужен потому, что показ САМ
// подталкивает закачку недостающего превью: без заглушки набор тестов ходил бы
// на i.ytimg.com, то есть падал бы в CI и врал бы на рабочей машине.
type offlineTransport struct{}

func (offlineTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("в тестах сети нет")
}

// usePreviews подменяет хранилище превью на временный каталог и возвращает
// функцию, кладущую туда готовый файл.
func usePreviews(t *testing.T) func(key string) {
	t.Helper()
	dir := t.TempDir()
	prev := previews
	previews = &previewStore{
		dir:    filepath.Join(dir, previewDir),
		client: &http.Client{Transport: offlineTransport{}},
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	t.Cleanup(func() { previews = prev })

	return func(key string) {
		t.Helper()
		path := filepath.Join(previews.dir, filepath.FromSlash(key)+previewExt)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("не важно, что внутри"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Факт: ролик опознаётся во всех формах адреса, которыми им делятся люди —
// длинной, короткой, «шортсом» и с меткой времени в хвосте.
func TestVideoURLFormsRecognised(t *testing.T) {
	const id = "dQw4w9WgXcQ"
	for _, addr := range []string{
		"https://www.youtube.com/watch?v=" + id,
		"https://youtube.com/watch?v=" + id + "&t=42s",
		"https://m.youtube.com/watch?v=" + id,
		"https://youtu.be/" + id,
		"https://youtu.be/" + id + "?t=42",
		"https://www.youtube.com/shorts/" + id,
		"https://www.youtube.com/embed/" + id,
		"https://www.youtube.com/live/" + id,
	} {
		r, ok := parseVideoURL(addr)
		if !ok {
			t.Errorf("%s: не опознан", addr)
			continue
		}
		if r.id != id {
			t.Errorf("%s: идентификатор %q, ждали %q", addr, r.id, id)
		}
		if r.p.name != "YouTube" {
			t.Errorf("%s: площадка %q", addr, r.p.name)
		}
	}
}

// Факт: Rutube опознаётся — он и есть главный довод за карточку, потому что не
// замедлен и вопроса о вывозе данных не задаёт.
func TestRussianVideoHostsRecognised(t *testing.T) {
	const addr = "https://rutube.ru/video/18673ceba8ab218accfaad74e84ec346/"
	r, ok := parseVideoURL(addr)
	if !ok {
		t.Fatalf("%s: не опознан", addr)
	}
	if r.p.name != "Rutube" {
		t.Errorf("площадка %q, ждали Rutube", r.p.name)
	}
}

// Факт: VK Видео НЕ опознаётся, и это решение по замеру (23.08.2026 с боевого
// хоста): превью анонимно оттуда не взять — `vk.com/oembed` отдаёт HTML-404, а
// страница ролика уводит редиректом на login.vk.ru.
//
// Тест сторожит не форму адреса, а то, чтобы площадку не вернули в список «на
// будущее»: ссылок на VK в комментариях больше, чем на все прочие вместе, и
// каждая заводила бы фоновую закачку, которая гарантированно не удаётся.
func TestVKIsDeliberatelyAbsent(t *testing.T) {
	for _, addr := range []string{
		"https://vk.com/video-104917606_456240565",
		"https://m.vk.com/video-31097370_456240393",
		"https://vkvideo.ru/video-12345_67890",
	} {
		if _, ok := parseVideoURL(addr); ok {
			t.Errorf("%s: опознан, хотя превью оттуда взять нечем", addr)
		}
	}
}

// Факт: хост сверяется ЦЕЛИКОМ, а не вхождением подстроки. Иначе чужой домен,
// начинающийся или кончающийся знакомыми буквами, поставил бы свою картинку на
// нашу страницу — тот же довод, что у isOwnLink.
func TestForeignHostIsNotAVideo(t *testing.T) {
	for _, addr := range []string{
		"https://youtube.com.evil.example/watch?v=dQw4w9WgXcQ",
		"https://evil.example/youtube.com/watch?v=dQw4w9WgXcQ",
		"https://notyoutube.com/watch?v=dQw4w9WgXcQ",
		"https://rutube.ru.evil.example/video/18673ceba8ab218accfaad74e84ec346/",
		"javascript:alert(1)//youtu.be/dQw4w9WgXcQ",
	} {
		if _, ok := parseVideoURL(addr); ok {
			t.Errorf("%s: опознан как ролик, а не должен", addr)
		}
	}
}

// Факт: идентификатор проверяется по форме, потому что он уходит в ИМЯ ФАЙЛА.
// Кривой отбрасывается целиком — карточки просто не будет.
func TestMalformedVideoIDRejected(t *testing.T) {
	for _, addr := range []string{
		"https://youtu.be/../../etc/passwd",
		"https://youtu.be/short",
		"https://www.youtube.com/watch?v=" + strings.Repeat("A", 40),
		"https://www.youtube.com/watch?v=",
		"https://www.youtube.com/",
		"https://rutube.ru/video/нехекс/",
	} {
		if _, ok := parseVideoURL(addr); ok {
			t.Errorf("%s: опознан как ролик, а не должен", addr)
		}
	}
}

// Факт: путь превью не выходит за свой каталог ни при каком опознанном ролике —
// и это следствие проверки выше, а не отдельной осторожности.
func TestPreviewPathStaysInsideDir(t *testing.T) {
	usePreviews(t)
	r, ok := parseVideoURL("https://youtu.be/dQw4w9WgXcQ")
	if !ok {
		t.Fatal("адрес не опознан")
	}
	path := filepath.Clean(previews.path(r))
	if !strings.HasPrefix(path, filepath.Clean(previews.dir)+string(filepath.Separator)) {
		t.Fatalf("превью легло мимо каталога: %s", path)
	}
}

// Факт: карточка появляется, только когда превью УЖЕ лежит у нас. Нет файла —
// нет карточки, и ссылка остаётся в теле текстом, как всякий чужой адрес.
func TestCardOnlyWhenPreviewIsStored(t *testing.T) {
	store := usePreviews(t)
	const body = "смотри https://youtu.be/dQw4w9WgXcQ вот"

	if got := videoCards(nativeID, body); len(got) != 0 {
		t.Fatalf("превью нет, а карточек %d", len(got))
	}
	store("yt/dQw4w9WgXcQ")
	got := videoCards(nativeID, body)
	if len(got) != 1 {
		t.Fatalf("превью лежит, а карточек %d", len(got))
	}
	if got[0].Preview != platform.MediaURLPrefix+"v/yt/dQw4w9WgXcQ.jpg" {
		t.Errorf("адрес превью %q — он обязан быть нашим", got[0].Preview)
	}
	if got[0].URL != "https://youtu.be/dQw4w9WgXcQ" {
		t.Errorf("клик ведёт на %q", got[0].URL)
	}
}

// Факт: в ЧУЖОМ тексте ссылка в карточку не разворачивается. Тот же рубеж, что
// у разметки и смайлов: своё разбираем целиком, зеркальное показываем так, как
// показывал его сайт, — а НГС роликов не разворачивал.
func TestMirroredTextGetsNoCards(t *testing.T) {
	store := usePreviews(t)
	store("yt/dQw4w9WgXcQ")
	const body = "смотри https://youtu.be/dQw4w9WgXcQ вот"

	if got := videoCards(312696, body); len(got) != 0 {
		t.Errorf("у зеркальной реплики %d карточек", len(got))
	}
	if got := videoCards(platform.RestoredIDBase+5, body); len(got) != 0 {
		t.Errorf("у восстановленной реплики %d карточек", len(got))
	}
	if got := videoCards(nativeID, body); len(got) != 1 {
		t.Errorf("у своей реплики %d карточек, ждали 1", len(got))
	}
}

// Факт: одна и та же ссылка, названная в реплике дважды, — это один ролик.
// Иначе цитата чужого сообщения удваивала бы карточку.
func TestRepeatedLinkGivesOneCard(t *testing.T) {
	store := usePreviews(t)
	store("yt/dQw4w9WgXcQ")
	body := "https://youtu.be/dQw4w9WgXcQ и ещё раз https://youtu.be/dQw4w9WgXcQ"
	if got := videoCards(nativeID, body); len(got) != 1 {
		t.Fatalf("карточек %d, ждали 1", len(got))
	}
}

// Факт: знак препинания после ссылки в неё не входит — «см. …XcQ.» это адрес и
// точка, а не адрес с точкой. Правило общее с обычными ссылками (linkTailCut),
// и проверяется здесь потому, что тут оно решает, найдётся ли файл превью.
func TestSentencePunctuationIsNotPartOfTheLink(t *testing.T) {
	store := usePreviews(t)
	store("yt/dQw4w9WgXcQ")
	if got := videoCards(nativeID, "смотри https://youtu.be/dQw4w9WgXcQ."); len(got) != 1 {
		t.Fatalf("карточек %d, ждали 1", len(got))
	}
}

// Факт: тело реплики от карточки не меняется — ссылка остаётся в тексте и
// видна целиком. Карточка ДОБАВЛЯЕТСЯ, а не подменяет собой адрес: правило
// «чужой адрес виден целиком» решением про ролики не отменялось.
func TestBodyKeepsTheLinkAsText(t *testing.T) {
	store := usePreviews(t)
	store("yt/dQw4w9WgXcQ")
	html := string(renderBody("", "смотри https://youtu.be/dQw4w9WgXcQ вот",
		era{markup: true, smiles: true, links: linkOwn}))
	if !strings.Contains(html, "https://youtu.be/dQw4w9WgXcQ") {
		t.Fatalf("адрес пропал из тела: %s", html)
	}
}

// Факт: без каталога медиа карточек нет вовсе, и страница ведёт себя ровно так,
// как до этой правки. Состояние рабочее, а не аварийное: на стенде без медиа
// морда обязана рисоваться.
func TestNoMediaDirNoCards(t *testing.T) {
	prev := previews
	previews = nil
	t.Cleanup(func() { previews = prev })

	if got := videoCards(nativeID, "https://youtu.be/dQw4w9WgXcQ"); got != nil {
		t.Fatalf("карточек %d, ждали ни одной", len(got))
	}
	videoWarm(nativeID, "https://youtu.be/dQw4w9WgXcQ") // не должен паниковать
}
