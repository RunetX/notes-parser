package narod

// ПОВЕРХНОСТЬ РЕЧИ — то, по чему машину узнают раньше, чем прочтут.
//
// Замер первой боевой песочницы (29.08.2026) против живого треда 257665 на ту же
// тему: длину калибровка выправила (медиана 61 против 66), а поверхность
// разошлась вся. Три четверти наших реплик кончались РОВНО ОДНОЙ скобкой при
// 16 % у живых; многоточия не было вовсе при 22–29 %; кодов смайлов сайта ноль
// при 10 %; строчной буквы в начале ноль при 12 %.
//
// Причина не в замере — он ЕСТЬ и лежит в карточке (ParenRuns, StartsLower,
// NoFinalPunct, SmileyRate). Причина в том, что до сегодня он уходил в промпт
// ОПИСАНИЕМ («точку в конце не ставишь в 75 % реплик»), а описание модель не
// исполняет: у Севы замерено 75 % реплик без точки, и он писал с точками все до
// одной. Это тот же урок, что уже дважды оплачен длиной и эмодзи, и лечится он
// тем же — не просьбой, а КОДОМ после генерации.
//
// Поэтому поверхность доводится ПОСТПРОЦЕССОРОМ, рядом с InjectErrors и по
// одному с ним доводу: у человека это не решение, а привычка, и она не меняется
// от того, о чём он сейчас пишет.

import (
	"math/rand/v2"
	"strings"
	"unicode"
)

// parenOrder — порядок корзин в ParenRuns; ключи те же, что в замере.
var parenOrder = []string{"1", "2", "3", "4+"}

// ApplyRegister доводит поверхность реплики до замеренной у донора: скобочная
// подпись, строчная в начале, точка в конце, смайлы сайта и многоточие.
//
// Детерминирована зерном, как InjectErrors: один и тот же план даёт одну и ту же
// реплику, иначе повтор после сбоя менял бы текст.
//
// ellipsis — доля реплик с многоточием. Число КОРПУСНОЕ (22,5 % по выборке из
// 111 тыс. реплик), а не донорское: в карточке его пока нет, и названо это
// честно — как только замер станет личным, число уедет в Register.
func ApplyRegister(text string, r Register, ellipsis float64, seed uint64) string {
	s := strings.TrimRight(text, " \t")
	if s == "" {
		return text
	}
	rng := rand.New(rand.NewPCG(seed^0x5deece66d, seed))

	s = applyParens(s, r.ParenRuns, rng)
	s = applyEnding(s, r.NoFinalPunct, ellipsis, rng)
	s = applySmiley(s, r, rng)
	s = applyCase(s, r, rng)
	return s
}

// applyParens приводит ХВОСТОВУЮ связку скобок к замеренной длине.
//
// Доли в ParenRuns — это доли ВСЕХ реплик, а не только скобочных, поэтому
// остаток до единицы означает «без скобок вовсе», и тогда хвост срезается.
// Скобки внутри текста не трогаются: там они бывают настоящими.
func applyParens(s string, runs map[string]float64, rng *rand.Rand) string {
	if len(runs) == 0 {
		return s
	}
	body := strings.TrimRight(s, ")")
	if body == s {
		return s // модель скобок не поставила — сама по себе это не ошибка
	}
	u, acc, want := rng.Float64(), 0.0, 0
	for i, k := range parenOrder {
		acc += runs[k]
		if u < acc {
			want = i + 1
			break
		}
	}
	if want == 0 {
		return body // замер говорит: в этой реплике скобок нет
	}
	return body + strings.Repeat(")", want)
}

// applyEnding решает судьбу точки в конце ОДНИМ броском: убрать, растянуть в
// многоточие или оставить. Тремя бросками подряд получилось бы «убрал и тут же
// растянул», то есть поведение, которого нет ни у кого.
func applyEnding(s string, noFinal, ellipsis float64, rng *rand.Rand) string {
	body := strings.TrimRight(s, ")")
	tail := s[len(body):]
	if !strings.HasSuffix(body, ".") || strings.HasSuffix(body, "..") {
		return s
	}
	switch u := rng.Float64(); {
	case u < noFinal:
		return strings.TrimRight(body, ".") + tail
	case u < noFinal+ellipsis:
		return body + ".." + tail
	}
	return s
}

// applySmiley дописывает код смайла САЙТА — не эмодзи: у эмодзи свой жребий и
// свой замер, а смайлы это отдельный словарь, живший на НГС до 2017 года.
func applySmiley(s string, r Register, rng *rand.Rand) string {
	if r.SmileyRate <= 0 || len(r.Smileys) == 0 {
		return s
	}
	if rng.Float64() >= r.SmileyRate {
		return s
	}
	// Из своих частых, а не из общего словаря: смайл у человека свой, как слово.
	top := r.Smileys
	if len(top) > 5 {
		top = top[:5]
	}
	code := top[rng.IntN(len(top))].Text
	if code == "" {
		return s
	}
	return s + " " + code
}

// applyCase роняет первую букву в строчную либо всю реплику целиком.
// AllLower — частный случай StartsLower, поэтому бросок один.
func applyCase(s string, r Register, rng *rand.Rand) string {
	if r.StartsLower <= 0 && r.AllLower <= 0 {
		return s
	}
	u := rng.Float64()
	switch {
	case u < r.AllLower:
		return strings.ToLower(s)
	case u < r.StartsLower:
		rs := []rune(s)
		rs[0] = unicode.ToLower(rs[0])
		return string(rs)
	}
	return s
}
