package narod

// Пайплайн генерации реплики — то, чем житель говорит в бою.
//
// Он ОТДЕЛЬНЫЙ от калибровочного (archive.GenerateVoice), и это не дубль.
// Калибровочный меряет ГОЛОС: строит по настоящим текстам донора held-out-полосу,
// прогоняет черновик через атрибутор архива и возвращает квантиль. Ему нужен
// archive.db на 4,7 ГБ — а боевой демон живёт без архива вовсе, и таскать его на
// хост площадки ради проверки, которую там всё равно нечем истолковать, никто не
// станет. Отсюда разделение труда: голос меряется ОДИН РАЗ, на калибровке, и
// результат вшивается в карточку числами; в бою остаются проверки, которым архив
// не нужен, — форма, приметы машины, стоп-лист имён, мат.
//
// Порядок в конвейере строгий и тот же, что записан в плане эпика:
//
//	генерация → sitetext.Normalize → евалы → ApplyRegister → InjectErrors → публикация
//
// Раньше евалов ошибки вносить нельзя (Normalize чинит ровно то, что мы вносим),
// позже публикации — поздно. После инъекции остаются только НЕОТКАТЫВАЮЩИЕ
// проверки: длина и пустота, то есть те, чей провал означает «не публиковать», а
// не «переписать».
//
// SKIP ОБЯЗАТЕЛЕН И ЧАСТ. Модель вправе ответить «мне здесь нечего сказать», и
// это не отказ службы, а поведение участника: кубик решил ПРИЙТИ, а вот есть ли
// с чем, видно только по самому треду. Приём и его обоснование — из амвона.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"lovegw/internal/profanity"
	"lovegw/internal/sitetext"
)

// WritePoint — всё, из чего собирается одна реплика.
type WritePoint struct {
	Card   *Card
	Note   StageNote
	Thread []StageReply
	// ReplyTo — реплика, которой отвечаем. Нулевой номер значит «пишем самой
	// заметке», и это другой жанр: адресата нет, обращения не бывает.
	ReplyTo StageReply
	Memory  string // блок «что ты помнишь» (recall.go)
	Mood    string // блок «с каким чувством пишешь» (mood.go)
	// Asks — спросить ли в ЭТОЙ реплике вместо утверждения. ЗАМЕР по 111 тыс.
	// реплик архива: вопрос стоит в 19,0 % (у двух живых тредов на ту же тему
	// 20,7 % и 21,2 %), у нас выходило 0–6 %. Жители утверждали и не
	// спрашивали — а разговор держится на вопросах: они и есть приглашение
	// ответить, то есть точки решения для остальных.
	Asks bool
	// OneThought — писать ли ЭТУ реплику одной фразой. Жребий, а не вкус: замер по
	// 111 тыс. реплик архива говорит, что одним предложением написаны 75,6 %, а у
	// нас выходило 15,6 % — каждая реплика была сетапом и панчем, и эта
	// двухчастность узнаётся раньше содержания.
	OneThought bool
	// Ellipsis — доля реплик с многоточием (корпусные 22,5 %; в карточке замера
	// пока нет, и число едет параметром, чтобы это было видно).
	Ellipsis float64
	// TargetRunes — длина ЭТОЙ реплики, выпавшая жребием из разброса карточки.
	// Ноль — длина не названа. Жребий бросает зовущий: разброс живёт МЕЖДУ
	// репликами, и модель, прочитавшая «обычно 58 знаков», пишет 58 всегда.
	TargetRunes int
	Emoji       *bool // есть ли эмодзи в этой реплике; nil — жребий не бросали
	// Digit — назвать ли в ЭТОЙ реплике число: цену, срок, год, возраст,
	// расстояние. ЗАМЕР (30.08.2026, internal/speech): у доноров число стоит в
	// 5,8–17,7 % реплик, по корпусу в 11,2 %, у нас выходило 1,0 % — в десять
	// раз реже. Это и есть добрая половина «абстрактности»: живой человек помнит
	// цену и год, а рассуждение их не содержит никогда.
	//
	// Жребий ОДНОСТОРОННИЙ, в отличие от односложности, и разница названа
	// нарочно: там умолчание модели было ВЫШЕ замера и приходилось называть оба
	// исхода, здесь оно почти ноль — запрещать нечего.
	Digit bool
	// Broad — вынести ли сказанное на всех. Оборотная сторона предыдущего и
	// замер того же дня: обобщение («все, всегда, вообще, люди») стоит у доноров
	// в 11,6–18,7 % реплик, у нас в 34,3 %. Здесь умолчание модели ВЫШЕ замера,
	// поэтому жребий ДВУСТОРОННИЙ: молчание о запрете оставило бы всё как было.
	//
	// Указателем, а не булевым: у карточек, снятых до замера, доля нулевая, и
	// «никогда не обобщай» было бы не замером, а выдумкой. nil — не бросали.
	Broad *bool
}

// Draft — что вышло.
type Draft struct {
	Text     string
	Skip     bool     // модель сказала «мне здесь нечего сказать»
	Reason   string   // почему пропустили или почему не вышло
	Attempts int      // сколько раз спрашивали модель
	Rejects  []string // брак по кругам — он и едет в gen_runs
}

// writeRetries — сколько раз переспрашивать при браке. Три: столько же у
// дайджеста и у утренней заметки, и по той же причине — вырожденный ответ бывает
// разовым, а четвёртая попытка уже платит за то, что не чинится.
const writeRetries = 3

// MaxReplyRunes — санитарный потолок реплики. Он не про стиль (за стиль отвечает
// TargetRunes из замера), а про то, чтобы сбой модели не выложил на страницу
// простыню: у площадки потолок вдесятеро больше, и упереться в него значило бы
// опубликовать эту простыню целиком.
const MaxReplyRunes = 2000

// Write просит у модели реплику и доводит её до публикуемого вида.
//
// Ошибки СЕТИ и модели возвращаются наружу: они означают «сейчас не вышло, и это
// не про текст». Брак же самого текста наружу не идёт — он превращается в Draft
// со Skip и причиной, потому что для службы «модель написала плохо трижды» и
// «житель промолчал» кончаются одинаково, а различить их обязан журнал.
func Write(ctx context.Context, gen JSONGenerator, p WritePoint, seed uint64) (Draft, error) {
	if gen == nil || p.Card == nil {
		return Draft{}, fmt.Errorf("генерация: нет модели или карточки")
	}
	var d Draft
	feedback := ""
	for attempt := 1; attempt <= writeRetries; attempt++ {
		d.Attempts = attempt
		raw, err := gen.GenerateJSON(ctx, writeSystem(p), writePrompt(p, feedback), writeSchema)
		if err != nil {
			return d, fmt.Errorf("реплика %s: %w", p.Card.ID, err)
		}
		var reply struct {
			Action string `json:"action"`
			Reason string `json:"reason"`
			Text   string `json:"text"`
		}
		if err := json.Unmarshal(raw, &reply); err != nil {
			return d, fmt.Errorf("реплика %s: разбор ответа: %w", p.Card.ID, err)
		}
		if reply.Action == "skip" {
			d.Skip, d.Reason = true, firstNonEmpty(reply.Reason, "нечего сказать")
			return d, nil
		}
		text := sitetext.Normalize(reply.Text)
		if bad := checkDraft(text, p); bad != "" {
			d.Rejects = append(d.Rejects, bad)
			feedback = bad
			continue
		}
		// Ошибки вносятся ПОСЛЕ проверок и до последнего осмотра: дальше остаются
		// только правила, чей провал означает «не публиковать».
		// Поверхность речи доводится ДО ошибок: ApplyRegister решает судьбу точки в
		// конце и скобочного хвоста, а InjectErrors правит слова внутри, — поменяй
		// их местами, и внесённая опечатка могла бы уехать вместе со снятой точкой.
		text = ApplyRegister(text, p.Card.Register, p.Ellipsis, seed)
		text = InjectErrors(text, p.Card.Errors, seed)
		if bad := checkFinal(text); bad != "" {
			d.Rejects = append(d.Rejects, bad)
			feedback = bad
			continue
		}
		d.Text = text
		return d, nil
	}
	d.Skip, d.Reason = true, "три черновика подряд не прошли проверки"
	return d, nil
}

// checkDraft — механические евалы черновика. Возвращает причину брака или пусто.
//
// Каждая причина названа ТЕКСТОМ, который годится и в промпт следующего круга, и
// в журнал дропов. Второй формулировки для человека не заводится — иначе однажды
// в gen_runs встанет причина, которой модель не видела.
func checkDraft(text string, p WritePoint) string {
	switch {
	case strings.TrimSpace(text) == "":
		return "пустая реплика"
	case utf8.RuneCountInString(text) > MaxReplyRunes:
		return fmt.Sprintf("реплика длиннее %d знаков", MaxReplyRunes)
	}
	if s := sitetext.MachineTell(text); s != "" {
		return "в тексте примета машины: " + s
	}
	if s := sitetext.MarkdownHit(text); s != "" {
		return "разметка markdown: " + s
	}
	if s := sitetext.TypographyHit(text); s != "" {
		return "типографика, которой на сайте не бывает: " + s
	}
	if s := sitetext.ForeignScript(text); s != "" {
		return "чужой алфавит: " + s
	}
	if s := sitetext.LatinFragment(text); s != "" {
		return "слово латиницей: " + s
	}
	if s := sitetext.JokeTag(text); s != "" {
		return "пометка о шутке: " + s
	}
	if s := profanity.FindMat(text); s != "" {
		return "мат («" + s + "») — жителям он запрещён: автомат модерации гасит брань сам, и такая реплика исчезнет со страницы"
	}
	// Эмодзи: жребий бросил зовущий, и модель обязана в него попасть. Без этого
	// она либо ставит их всегда, либо, как вышло на замере 28.08.2026, не ставит
	// вовсе — а доля живёт МЕЖДУ репликами и одной репликой невыразима.
	if p.Emoji != nil {
		if got := sitetext.CountEmoji(text) > 0; got != *p.Emoji {
			if *p.Emoji {
				return "в этой реплике должен быть эмодзи, а его нет"
			}
			return "в этой реплике эмодзи быть не должно"
		}
	}
	if name := strangeName(text, p); name != "" {
		return "имя, которого в этом разговоре нет: " + name
	}
	return ""
}

// checkFinal — то, что смотрят ПОСЛЕ внесения ошибок. Список короткий намеренно:
// всё, что чинится переписыванием, обязано отсеяться до инъекции, иначе мы будем
// чинить собственную порчу.
func checkFinal(text string) string {
	if strings.TrimSpace(text) == "" {
		return "после внесения ошибок реплика опустела"
	}
	if utf8.RuneCountInString(text) > MaxReplyRunes {
		return fmt.Sprintf("после внесения ошибок реплика длиннее %d знаков", MaxReplyRunes)
	}
	return ""
}

// strangeName — капитализированное слово, которого нет ни среди участников
// разговора, ни в биографии жителя.
//
// Урок оплачен инцидентом со случайным именем: модель, которой не за что
// зацепиться, зовёт собеседника первым попавшимся русским именем, и на площадке
// это читается как обращение к человеку, которого там нет. Проверка грубая — она
// пропустит имя, совпавшее с ником, и придерётся к названию улицы, — но цена
// ошибки несимметрична: лишний круг генерации стоит центы, а чужое имя под
// репликой не отменяется.
func strangeName(text string, p WritePoint) string {
	known := map[string]bool{}
	add := func(s string) {
		for _, w := range sitetext.Words(s) {
			known[strings.ToLower(w)] = true
		}
	}
	add(p.Card.Persona.Nick)
	add(p.Card.Persona.Family)
	add(p.Card.Persona.City)
	add(p.Card.Persona.Job)
	for _, f := range p.Card.Persona.Facts {
		add(f)
	}
	add(p.Note.AuthorNick)
	add(p.Note.Body)
	for _, c := range p.Thread {
		add(c.AuthorNick)
		add(c.Body)
	}
	head := strings.TrimSpace(text)
	for _, w := range sitetext.Words(text) {
		r := []rune(w)
		if len(r) < 3 || !unicode.IsUpper(r[0]) || known[strings.ToLower(w)] {
			continue
		}
		// Слово с заглавной В НАЧАЛЕ — обычное дело; ловим то, что стоит в
		// середине и потому названо намеренно.
		if strings.HasPrefix(head, w) {
			continue
		}
		if looksLikeName(w) {
			return w
		}
	}
	return ""
}

// looksLikeName — похоже ли слово на личное имя: кириллица, одна заглавная в
// начале. Аббревиатуры, латиница и коды смайлов уже отсеяны соседними
// проверками, поэтому здесь достаточно формы слова.
func looksLikeName(w string) bool {
	for i, r := range w {
		switch {
		case !unicode.Is(unicode.Cyrillic, r):
			return false
		case i > 0 && unicode.IsUpper(r):
			return false
		}
	}
	return true
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
