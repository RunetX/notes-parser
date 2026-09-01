package narod

// ЗАРАЖЕНИЕ КАНОНОМ — выдуманный факт, который приняли все.
//
// Разбор первого треда с панчем (01.09.2026, заметка 313138): жители
// сговорились о «четырёх годах» и «чужой свадьбе», которых в заметке нет
// вовсе. Один сказал — остальные приняли за известное и достроили. У живых
// подхватывают двое-трое, а дальше разговор идёт своей дорогой, и это ровно
// та мера, которую владелец назвал в приёмке: каждый выдуманный факт — не
// больше чем у трёх жителей.
//
// ЧИНИТСЯ ЭТО НЕ ОКНОМ. Первое, что приходит в голову, — показывать жителю не
// весь тред, а случайное подмножество; но 31.08.2026 конвергенцию уже списывали
// на окно и замер это ОПРОВЕРГ: в треде на 400 реплик окно составляет пять
// процентов, а общее слово стояло как стояло. К тому же threadLimit обязан
// совпадать с калибровочным branchLimit — иначе голос мерился бы при одном
// объёме контекста, а писался при другом. Поэтому здесь режется не ОКНО, а сам
// повтор: слово, уже сказанное тремя разными жителями и отсутствующее в
// заметке, следующему запрещается.
//
// Слово, а не «факт», — и это граница метода, названная заранее: невод ловит
// повторяющееся СЛОВО. Он не знает, что «четыре года» и «четвёртый год» одно и
// то же, и не понимает, что именно жители придумали. Зато он мёртво считает
// то, ради чего заведён: сколько РАЗНЫХ людей повторили одно и то же слово,
// которого в заметке не было.

import (
	"sort"
	"strings"

	"lovegw/internal/speech"
)

// canonMinAuthors — сколько разных жителей должны сказать слово, чтобы
// следующему оно было запрещено. Трое: приёмка владельца — «подхвачен не
// больше чем тремя», значит запрет ложится на четвёртого.
const canonMinAuthors = 3

// canonCap — сколько слов показывать модели. Восемь: список — это запрет, а
// длинный запрет модель читает как тему («не думай о белой обезьяне»), и
// каждая лишняя строка ещё и оплачивается на каждом круге.
const canonCap = 8

// canonWords — слова, пошедшие по кругу: их нет в заметке, но их сказали уже
// canonMinAuthors разных жителей.
//
// mine — тот, кто пишет сейчас: СВОЁ слово ему не запрещается. Человек, дважды
// помянувший собственную историю, — это человек, а не заражение; запрещать ему
// собственные слова значило бы лечить память вместо повтора.
func canonWords(note StageNote, thread []StageReply, mine int64) []string {
	known := map[string]bool{}
	for _, w := range speech.ContentWords(note.Body) {
		known[w] = true
	}
	// Ники участников каноном не бывают: обращение по имени — это разговор, а
	// не выдуманный факт. Ник автора заметки тоже: он и есть предмет.
	for _, n := range append([]string{note.AuthorNick}, nicksOf(thread)...) {
		for _, w := range speech.ContentWords(n) {
			known[w] = true
		}
	}

	authors := map[string]map[int64]bool{}
	for _, c := range thread {
		for _, w := range speech.ContentWords(c.Body) {
			if known[w] {
				continue
			}
			if authors[w] == nil {
				authors[w] = map[int64]bool{}
			}
			authors[w][c.AuthorID] = true
		}
	}

	type hit struct {
		word string
		n    int
	}
	var hits []hit
	for w, who := range authors {
		if len(who) < canonMinAuthors || who[mine] {
			continue
		}
		hits = append(hits, hit{w, len(who)})
	}
	// По убыванию числа подхвативших, при равенстве — по алфавиту: список
	// уходит в промпт, и он обязан быть одинаковым при одинаковом треде.
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].n != hits[j].n {
			return hits[i].n > hits[j].n
		}
		return hits[i].word < hits[j].word
	})
	if len(hits) > canonCap {
		hits = hits[:canonCap]
	}
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.word)
	}
	return out
}

// nicksOf — ники всех, кто говорил в треде.
func nicksOf(thread []StageReply) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range thread {
		if c.AuthorNick == "" || seen[c.AuthorNick] {
			continue
		}
		seen[c.AuthorNick] = true
		out = append(out, c.AuthorNick)
	}
	return out
}

// canonHit — какое из запрещённых слов житель всё-таки повторил; пусто — ни
// одного. Сверка по СЛОВАМ текста, а не подстрокой: «свадьба» не должна
// находиться внутри «свадьбами» по случайности разбора, а находиться должна по
// разбору — тем же ContentWords, каким слово в список и попало.
func canonHit(text string, canon []string) string {
	if len(canon) == 0 {
		return ""
	}
	said := map[string]bool{}
	for _, w := range speech.ContentWords(text) {
		said[w] = true
	}
	for _, w := range canon {
		if said[strings.ToLower(w)] {
			return w
		}
	}
	return ""
}
