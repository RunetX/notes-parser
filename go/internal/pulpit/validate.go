package pulpit

// Проверка того, что вернула модель. Функция чистая: текст уходит на сайт
// необратимо, и единственный способ проверить эти правила — таблица случаев, а
// не боевая реплика.
//
// Разделение труда такое: механическое и однозначное чиним подстановкой
// (`sitetext.Normalize`), спорное — бракуем и переспрашиваем (validate). Лишний
// круг к модели стоит 10–30 секунд, то есть ровно той валюты, ради которой всё
// затевалось, поэтому тире и кавычки не повод для переспроса. Валидатор их всё
// равно проверяет — как страховку от дырки в нормализации, а не как рабочую
// дорогу.
//
// Правила, которые задаёт САЙТ (типографика, NBSP, BB-коды, HTML, обломки
// генерации), живут в общем `internal/sitetext`: с 24.08.2026 их же соблюдает
// утренняя заметка, а две копии одних правил разошлись бы молча. Здесь остаётся
// то, что про саму РЕПЛИКУ: афоризм вместо шутки, рисунок из знаков, пересказ
// заметки, обращение «Ник,».

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"lovegw/internal/love"
	"lovegw/internal/sitetext"
)

// Ширина и высота рисунка из знаков. Шрифт на площадке пропорциональный:
// широкая картинка развалится, поэтому годятся разделители и силуэты, а не
// блочная графика с выравниванием по колонкам.
const (
	maxArtWidth = 30
	maxArtLines = 5
)

// maxOverlapShare — доля общих пятёрок слов с заметкой, после которой это уже
// пересказ, а не отклик.
const maxOverlapShare = 0.2

// replyTells — приметы несмешного. Сами выражения живут в `sitetext` вместе с
// остальными приметами нашей генерации: сползает в них ЛЮБОЙ текст, который
// пишет модель, и вторая копия у утренней заметки разошлась бы с этой молча.
// Здесь остаётся только формулировка причины — она едет в промпт переспроса, а
// говорить с редактором реплики надо про реплику.
var replyTells = []struct {
	find   func(string) string
	reason string
}{
	{sitetext.JokeTag, "метка шутки «%s»: если смешно, она не нужна, если не смешно — не спасёт"},
	{sitetext.Aphorism, "афоризм «%s»: это наблюдение сверху, а не шутка — влезь внутрь ситуации"},
}

// tellHit — первая сработавшая примета ("" — чисто).
func tellHit(text string) string {
	if m := sitetext.MachineTell(text); m != "" {
		return m
	}
	for _, t := range replyTells {
		if m := t.find(text); m != "" {
			return fmt.Sprintf(t.reason, m)
		}
	}
	return ""
}

// hookPrefix — сколько первых рун слова сравниваем, проверяя, что деталь взята
// из заметки. Не всё слово: русский язык склоняет, и «инжир с хозяйского куста»
// против «хозяйский куст» в заметке — та же деталь. Четыре руны ловят корень
// (склонение правит хвост, а не начало) и не требуют от нас морфологии. Слова
// короче четырёх рун в расчёт не берутся вовсе: «и», «под», «куст» доказывают
// не больше, чем совпадение по алфавиту.
const hookPrefix = 4

// hookInNote — названа ли деталь словами автора. Достаточно одного слова: модель
// пересказывает деталь своей фразой, и требовать дословного совпадения целиком
// значило бы жечь переспросы на склонениях.
func hookInNote(hook, note string) bool {
	if strings.TrimSpace(hook) == "" || strings.TrimSpace(note) == "" {
		return true // нечего проверять
	}
	lower := strings.ToLower(note)
	judged := false
	for _, w := range sitetext.Words(hook) {
		r := []rune(w)
		if len(r) < hookPrefix {
			continue // короткие слова («и», «под», «куст») ничего не доказывают
		}
		judged = true
		if strings.Contains(lower, string(r[:hookPrefix])) {
			return true
		}
	}
	// Длинных слов не нашлось — судить не по чему, пропускаем.
	return !judged
}

// validateConfig — пороги проверки одной реплики.
type validateConfig struct {
	MinRunes   int
	MaxRunes   int
	MaxLines   int
	AllowEmoji bool
	// Forms — разрешённые формы; пусто — форма не спрашивается (ответ на ответ).
	Forms []string
	// NoteText — текст заметки: по нему ловится пересказ.
	NoteText string
	// Nicks — ники, обращение к которым модель писать не должна: их
	// подставляет инструмент (свой ник, имя автора заметки, ник собеседника).
	Nicks []string
}

// validate возвращает причину брака ("" — годится). Причина едет хвостом в
// промпт переспроса, поэтому формулируется как указание, а не как код ошибки.
func validate(q quip, cfg validateConfig) string {
	text, form := q.Text, q.Form
	if strings.TrimSpace(text) == "" {
		return "пустой текст"
	}
	if n := utf8.RuneCountInString(text); n < cfg.MinRunes || n > cfg.MaxRunes {
		return "длина " + strconv.Itoa(n) + " знаков вне допустимых " +
			strconv.Itoa(cfg.MinRunes) + "–" + strconv.Itoa(cfg.MaxRunes)
	}
	if tell := tellHit(text); tell != "" {
		// Сюда попадают и служебный тег размышления, и обрывок хода мысли, и
		// разметка, которая на сайте напечаталась бы буквально.
		return tell
	}
	if md := sitetext.MarkdownHit(text); md != "" {
		return "markdown (" + md + "): на сайте он напечатается буквально"
	}
	if bad := sitetext.TypographyHit(text); bad != "" {
		return "знак " + bad + " на площадке почти не встречается — только дефис и обычная кавычка"
	}
	lines := strings.Split(text, "\n")
	if cfg.MaxLines > 0 && len(lines) > cfg.MaxLines {
		return "строк " + strconv.Itoa(len(lines)) + ", потолок " + strconv.Itoa(cfg.MaxLines)
	}
	if reason := checkArt(lines); reason != "" {
		return reason
	}
	if frag := tailFragment(text); frag != "" {
		return "обрывок «" + frag + "» в конце: после панча не остаётся ничего"
	}
	if frag := sitetext.LatinFragment(text); frag != "" {
		return "латинский огрызок «" + frag + "» в русском тексте: мусор генерации"
	}
	if !cfg.AllowEmoji && sitetext.HasEmoji(text) {
		return "эмодзи, а в этот раз без них"
	}
	if p := love.AddressPrefix(text); p != "" && knownNick(p, cfg.Nicks) {
		// Обращение подставляет инструмент — так ник нельзя выдумать. Ловим
		// только НАСТОЯЩИЕ ники: по фикстуре живого треда видно, что сайт
		// выделяет жирным именно их, а обычный оборот «Смирение не в том, …»
		// он не трогает. Бить по любому слову перед запятой значило бы гонять
		// переспросы на ровном месте, а каждый стоит 10–30 секунд.
		return "обращение «" + p + ",» в начале: его подставляет инструмент, а не ты"
	}
	if share := overlapShare(text, cfg.NoteText); share > maxOverlapShare {
		return "пересказ заметки: слишком много общих оборотов"
	}
	if len(cfg.Forms) > 0 && !hasForm(cfg.Forms, form) {
		return "приём «" + form + "» не из предложенных (" + strings.Join(cfg.Forms, ", ") + ")"
	}
	if !hookInNote(q.Hook, cfg.NoteText) {
		// Деталь названа, но её в заметке нет — значит шутка выросла из темы
		// вообще (или из выдуманного факта), а не из того, что автор написал.
		return "детали «" + q.Hook + "» в заметке нет: цепляйся за то, что написал автор"
	}
	return ""
}

// tailFragment — обрывок в конце ("" — чисто). Последняя строка из одного-двух
// слов, и все они уже были выше: так выглядит хвост, оставшийся от правки
// («…Папа не уточнил, с какой.\nгрядки.» — живой черновик 16.08.2026), а не
// добивка. У настоящей добивки есть хотя бы одно своё слово, поэтому «Уже нет.»
// после диалога проходит, а повтор уже сказанного — нет.
func tailFragment(text string) string {
	lines := strings.Split(text, "\n")
	last := len(lines) - 1
	for last >= 0 && strings.TrimSpace(lines[last]) == "" {
		last--
	}
	if last <= 0 {
		return "" // одна строка: обрывку неоткуда взяться
	}
	tail := sitetext.Words(lines[last])
	if len(tail) == 0 || len(tail) > 2 {
		return ""
	}
	before := sitetext.Words(strings.Join(lines[:last], " "))
	for _, w := range tail {
		if !contains(before, w) {
			return "" // своё слово — значит добивка, а не хвост
		}
	}
	return strings.TrimSpace(lines[last])
}

// knownNick — совпало ли обращение с одним из тех ников, которые подставляет
// инструмент. Сравнение в нижнем регистре: AddressPrefix его уже привёл.
func knownNick(prefix string, nicks []string) bool {
	for _, n := range nicks {
		if n != "" && strings.EqualFold(strings.TrimSpace(n), prefix) {
			return true
		}
	}
	return false
}

// checkArt проверяет рисунок из знаков: он не должен быть шире 30 знаков и
// выше 5 строк подряд.
func checkArt(lines []string) string {
	block := 0
	for _, line := range lines {
		if !artLike(line) {
			block = 0
			continue
		}
		block++
		if w := utf8.RuneCountInString(strings.TrimSpace(line)); w > maxArtWidth {
			return "рисунок шире " + strconv.Itoa(maxArtWidth) + " знаков — на пропорциональном шрифте он развалится"
		}
		if block > maxArtLines {
			return "рисунок выше " + strconv.Itoa(maxArtLines) + " строк"
		}
	}
	return ""
}

// artLike — строка из знаков, а не из слов: в ней почти нет букв и цифр.
func artLike(line string) bool {
	trimmed := strings.TrimSpace(strings.ReplaceAll(line, string(sitetext.NBSP), " "))
	if utf8.RuneCountInString(trimmed) < 3 {
		return false
	}
	letters, total := 0, 0
	for _, r := range trimmed {
		if unicode.IsSpace(r) {
			continue
		}
		total++
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			letters++
		}
	}
	if total == 0 {
		return false
	}
	return float64(letters)/float64(total) < 0.2
}

// overlapShare — доля пятёрок слов реплики, дословно встречающихся в заметке.
// Пятёрка (а не тройка) выбрана как порог узнавания: три общих слова бывают
// случайно, пять подряд — это уже цитата.
func overlapShare(text, note string) float64 {
	const n = 5
	reply := sitetext.Words(text)
	if len(reply) < n {
		return 0
	}
	src := sitetext.Words(note)
	if len(src) < n {
		return 0
	}
	seen := make(map[string]bool, len(src))
	for i := 0; i+n <= len(src); i++ {
		seen[strings.Join(src[i:i+n], " ")] = true
	}
	total, shared := 0, 0
	for i := 0; i+n <= len(reply); i++ {
		total++
		if seen[strings.Join(reply[i:i+n], " ")] {
			shared++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(shared) / float64(total)
}

// hasForm — назвала ли модель одну из предложенных форм. Сравнение через
// formKey: «ложная серьезность» без «ё» — та же форма, а не повод переспрашивать.
func hasForm(forms []string, form string) bool {
	key := formKey(form)
	for _, f := range forms {
		if formKey(f) == key {
			return true
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
