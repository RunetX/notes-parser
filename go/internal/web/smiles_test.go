package web

import (
	"strings"
	"testing"
	"time"

	"lovegw/internal/platform"
)

// smileEra — время, когда смайлы сайт показывал, а разметку уже нет: рубежи
// разные, и тест на смайлы не должен зависеть от разбора [b].
var smileEra = time.Date(2016, 5, 1, 12, 0, 0, 0, time.UTC)

func smileNote(text string) string {
	return string(noteBodyHTML(platform.NoteView{ID: 300000, Body: text, PublishedAt: smileEra}))
}

// Свой набор знаков сайта, и в архиве он заметнее BB-кодов: у 6–11 %
// комментариев 2013–2017 годов есть хотя бы один смайл.
func TestSmileyRendered(t *testing.T) {
	got := smileNote("сижу читаю :::popcorn::: и жду")
	if !strings.Contains(got, `<img class="sm" src="/assets/smile/popcorn.`) {
		t.Fatalf("смайл не подставлен: %s", got)
	}
	// Размер у каждого свой и читается из самого файла — без атрибутов строка
	// дёргается, пока картинка грузится.
	if !strings.Contains(got, `width="35" height="35"`) {
		t.Errorf("размер картинки не проставлен: %s", got)
	}
	// alt — сам код: скопированный со страницы текст остаётся тем, что человек
	// написал в 2016-м.
	if !strings.Contains(got, `alt=":::popcorn:::"`) {
		t.Errorf("в alt не код смайла: %s", got)
	}
}

// После сентября 2017-го сайт эти коды показывал текстом. Значит и мы: переезд
// не место для улучшений оригинала.
func TestSmileyAfterSunsetStaysText(t *testing.T) {
	after := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	got := string(noteBodyHTML(platform.NoteView{ID: 312811, Body: "жду :::popcorn:::", PublishedAt: after}))
	if got != "<p>жду :::popcorn:::</p>" {
		t.Errorf("смайл подставлен там, где сайт его не показывал: %s", got)
	}
}

// Восемь кодов из архива сайт не отдаёт вовсе (опечатки авторов вроде
// «:::cofee:::»), и на НГС они тоже оставались текстом.
func TestUnknownSmileyStaysText(t *testing.T) {
	if got := smileNote("неизвестный :::cofee::: код"); got != "<p>неизвестный :::cofee::: код</p>" {
		t.Errorf("неизвестный код дал %q", got)
	}
}

// Два смайла подряд — обычное дело, и «:::» между ними принадлежит обоим кодам.
func TestSmileysBackToBack(t *testing.T) {
	got := smileNote(":::boogi::::::agree:::")
	if n := strings.Count(got, "<img "); n != 2 {
		t.Errorf("подставлено картинок: %d — %s", n, got)
	}
	if strings.Contains(got, ":::") && !strings.Contains(got, `alt=":::`) {
		t.Errorf("в тексте остались лишние двоеточия: %s", got)
	}
}

// Экранирование первично и здесь: подставляем мы только СВОИ адреса из статики,
// а всё, что пришло из базы, уходит в escape — включая код в alt.
func TestSmileyKeepsEscaping(t *testing.T) {
	got := smileNote(`<b>жирный</b> :::agree:::`)
	if strings.Contains(got, "<b>") {
		t.Fatalf("в разметку прошёл тег: %s", got)
	}
	if !strings.HasPrefix(got, "<p>&lt;b&gt;") {
		t.Errorf("текст не экранирован: %s", got)
	}
}

// Разметка и смайлы разбираются вместе там, где вместе жили: до 02.06.2014.
func TestSmileyInsideMarkup(t *testing.T) {
	got := legacyNote("[b]ура :::flowers:::[/b]")
	if !strings.HasPrefix(got, "<p><b>ура <img class=\"sm\"") || !strings.HasSuffix(got, "</b></p>") {
		t.Errorf("смайл внутри разметки собрался как %q", got)
	}
}

// Написанное на площадке под правила сайта не попадает вовсе: своего синтаксиса
// у нас нет, и «:::popcorn:::» здесь просто текст.
func TestNativeTextHasNoSmileys(t *testing.T) {
	if got := plainNote("наши :::popcorn::: коды"); got != "<p>наши :::popcorn::: коды</p>" {
		t.Errorf("нативный текст разобран как сайтовый: %s", got)
	}
}

// Картинки вшиты в бинарник и адресуются хешем содержимого, как вся статика.
func TestSmileAssetsEmbedded(t *testing.T) {
	if len(smiles) != 64 {
		t.Errorf("картинок смайлов %d, ожидалось 64", len(smiles))
	}
	for code, s := range smiles {
		if s.w == 0 || s.h == 0 {
			t.Errorf("%s: размер не прочитан из файла", code)
		}
		if _, ok := assets[strings.TrimPrefix(s.url, "/assets/")]; !ok {
			t.Errorf("%s: файла %s нет в статике", code, s.url)
		}
	}
}
