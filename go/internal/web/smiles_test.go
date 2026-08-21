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

// Зеркальный текст ПОСЛЕ заката знаки сайта не разбирает: в 2020-м на НГС этот
// код печатался буквально, значит и здесь он текст. Своё написанное — другое
// дело (см. TestNativeTextRendersSmileysButNotMarkup): там кнопку предлагаем мы.
func TestMirroredTextAfterSunsetKeepsCodes(t *testing.T) {
	if got := plainNote("наши :::popcorn::: коды"); got != "<p>наши :::popcorn::: коды</p>" {
		t.Errorf("зеркальный текст после заката разобран: %s", got)
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

// Написанное ЗДЕСЬ знает и смайлы, и разметку: площадка предлагает их сама
// (выбиралка и справочник под формой), а код, оставшийся на экране скобками, —
// это человек нажал кнопку и не получил обещанного. Рубежи сайта своему тексту
// не указ: у него свои правила, и они шире.
func TestNativeTextRendersSmileysAndMarkup(t *testing.T) {
	n := platform.NoteView{ID: platform.NativeIDBase + 7, Body: "[b]наши[/b] :::popcorn:::", PublishedAt: now}
	got := string(noteBodyHTML(n))
	if !strings.Contains(got, `<img class="sm"`) {
		t.Errorf("смайл в своём тексте не подставлен: %s", got)
	}
	if !strings.Contains(got, "<b>наши</b>") {
		t.Errorf("BB-код в своём тексте не разобран: %s", got)
	}
}

// Выбиралка отдаёт ВЕСЬ набор, и частые знаки стоят первыми: искать popcorn
// глазами в алфавитном списке из шестидесяти четырёх картинок — работа, которой
// можно не быть.
func TestSmileListIsCompleteAndFrequentFirst(t *testing.T) {
	list := smileList()
	if len(list) != len(smiles) {
		t.Errorf("в выбиралке %d знаков из %d", len(list), len(smiles))
	}
	if list[0] != "crazy2" || list[1] != "agree" {
		t.Errorf("порядок начинается с %q, %q", list[0], list[1])
	}
	seen := map[string]bool{}
	for _, code := range list {
		if seen[code] {
			t.Fatalf("код %s в списке дважды", code)
		}
		seen[code] = true
	}
}
