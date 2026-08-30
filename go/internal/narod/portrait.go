package narod

// Аватар жителя: английский промпт для генератора изображений.
//
// Понадобилось потому, что фото жителю взять НЕОТКУДА. Живому человеку его
// приносит анкета НГС, у персонажа анкеты нет вовсе, и вторая дверь для файла
// на площадке (`/mod/admin`) заведена ровно под этот случай. Дверь есть,
// картинки нет — вот её и надо чем-то нарисовать.
//
// ГЛАВНОЕ ОБ ИСХОДНЫХ ДАННЫХ: в карточке жителя ВНЕШНОСТИ НЕТ. Есть возраст,
// город, ремесло, семья и факты — и ни слова о том, как человек выглядит.
// Значит внешность не берётся, а ВЫВОДИТСЯ, и вся система про то, откуда что
// выводить: что считает код, что спрашивают у модели и чего не спрашивают
// никогда.
//
// РАЗДЕЛЕНИЕ ТРУДА — КАМЕРА И ЧЕЛОВЕК.
//
//   - КАМЕРА (ракурс, выражение, свет, фон) считается КОДОМ, жребием по ключу
//     жителя. Причина та же, по которой длина реплики и эмодзи ставятся кодом:
//     это величина, живущая МЕЖДУ портретами, а не внутри одного. Попроси
//     модель «сделай тридцать разных» — она сделает тридцать одинаковых, потому
//     что каждый промпт она видит поодиночке. Жребий же по ключу жителя даёт
//     набор, который не похож на паспортную серию, и даёт его ПОВТОРЯЕМО: тот
//     же житель — тот же кадр, сколько раз ни перегенерируй.
//
//   - ЧЕЛОВЕК (сложение, лицо, одежда) спрашивается у МОДЕЛИ, и это не лень
//     кода. «Рост под два метра, отсюда и ник» и «красит волосы в новый цвет
//     каждые два месяца» — свободный русский текст, и превратить его в
//     английскую строку про внешность значит понять, что там сказано. Ровно
//     та работа, ради которой модель и зовут: там, где нужно суждение о
//     СМЫСЛЕ, а не исполнение замера.
//
// ЧЕГО МОДЕЛЬ НЕ ВИДИТ, и это держит структура, а не дисциплина. В запрос идёт
// не Bio, а portraitFacts — отдельный тип, собираемый одной функцией factsFor.
// Не идут:
//
//   - НИК. Он ничего не говорит о лице, зато у слепка это ник ЖИВОГО ЧЕЛОВЕКА,
//     и просить по нему портрет значило бы рисовать лицо настоящему участнику.
//   - FRUSTRATION. Тот же довод, что у PublicBio: это рычаг кубика, а не факт о
//     человеке. Портрет, нарисованный «по больному месту», превратил бы
//     характер в приложенную к лицу инструкцию.
//   - Samples, Vocab, Register — их в Bio нет вовсе, и функция берёт Bio, а не
//     Card, именно поэтому: у слепка в Samples лежат ДОСЛОВНЫЕ письма живых
//     людей, и путь, по которому они дошли бы до запроса к Claude, лучше не
//     закрывать проверкой, а не заводить.
//
// ВИД — ФОТОРЕАЛИСТИЧНЫЙ, и это АВТОРСКОЕ РЕШЕНИЕ ВЛАДЕЛЬЦА (30.08.2026),
// принятое из трёх названных вариантов: рисованный портрет, единая обработка
// серии, фотореализм. Записано здесь, а не в конфиге, по доводу AutoHideable:
// величину без замера ещё можно обсуждать, пока она стоит одной строкой в коде
// и её правку видно в diff, — а флаг с одним значением это выдуманная гибкость.
//
// Цена решения названа вслух и тоже здесь: аватар жителя НЕ ОТЛИЧИТЬ от аватара
// живого участника, потому что рядом в ленте стоят настоящие фотографии с НГС.
// Значит вся тяжесть «читатель должен знать, что перед ним машина» ложится на
// значок песочницы и раздел /help#narod, и снимать их нельзя.
//
// И ОТДЕЛЬНО ПРО «ОБЫЧНОЕ ЛИЦО». Промпт настойчиво просит человека невзрачного,
// с возрастом на лице и несовершенной кожей. Это не украшение: генераторы
// тянут к модельной внешности, и тридцать красивых лиц подряд выдают машину
// РАНЬШЕ любой стилистики — на сайте знакомств так не выглядит никто. Тот же
// урок, что с поверхностью речи: машину выдаёт не содержание, а ровность.

import (
	"fmt"
	"math/rand/v2"
	"strings"
)

// portraitSalt — своя соль, чтобы жребий камеры не совпадал с жребием
// внутренних событий у того же жителя.
const portraitSalt = 0xA24BAED4963EE407

// Look — то, что модель рассказала о внешности. Три коротких английских куска,
// и делятся они так не для красоты: одежда и примета живут в промпте на разных
// местах, а пустая примета — законный случай (не у всякого жителя в фактах есть
// что-то видимое глазом).
type Look struct {
	// Person — сложение, лицо, волосы: «a tall, heavy-set man with a broad
	// face and short grey hair».
	Person string `json:"person"`
	// Clothing — во что одет, без брендов и надписей.
	Clothing string `json:"clothing"`
	// Detail — ОДНА видимая примета из фактов, если она там есть. Пусто —
	// нормально, и пустое место в промпте не остаётся.
	Detail string `json:"detail"`
}

// PortraitSchema — контракт ответа модели.
var PortraitSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"person":   map[string]any{"type": "string"},
		"clothing": map[string]any{"type": "string"},
		"detail":   map[string]any{"type": "string"},
	},
	"required":             []string{"person", "clothing", "detail"},
	"additionalProperties": false,
}

// portraitFacts — РОВНО то, что уходит модели. Отдельный тип, а не Bio, и это
// вся защита разом: чего в нём нет, того в запросе не будет никогда, сколько бы
// полей ни завелось у Bio потом.
type portraitFacts struct {
	Gender string
	Age    int
	City   string
	Job    string
	Facts  []string
}

// factsFor — единственное место, где решается, что модель узнает о жителе.
func factsFor(b Bio) portraitFacts {
	return portraitFacts{
		Gender: b.Gender,
		Age:    b.Age,
		City:   b.City,
		Job:    b.Job,
		Facts:  b.Facts,
	}
}

// ---------------------------------------------------------------- камера

// Таблицы жребия. Пять независимых рычагов дают набор, в котором ни один кадр
// не повторяет соседний, — а внутри каждой таблицы значения нарочно СКУЧНЫЕ:
// эффектный ракурс на ста пикселях не читается вовсе, зато выдаёт постановку.
var (
	portraitAngles = []string{
		"facing the camera straight on",
		"turned slightly to the left, eyes on the camera",
		"turned slightly to the right, eyes on the camera",
		"a three-quarter view, eyes on the camera",
		"facing the camera with the chin slightly lowered",
	}
	portraitFaces = []string{
		"a calm, neutral expression",
		"the faint beginning of a smile",
		"a warm, open expression",
		"a tired but friendly expression",
		"a guarded, thoughtful expression",
		"a slightly amused, sceptical expression",
	}
	portraitLight = []string{
		"Soft window light from the left",
		"Flat overcast daylight",
		"Warm indoor lamp light from one side",
		"Even diffused light with no strong shadows",
		"Low late-afternoon sun through a window",
	}
	// Без артикля: строка встаёт в «Plain %s background», и «a» посреди неё
	// давала «Plain a soft off-white background».
	portraitBack = []string{
		"warm grey",
		"cool grey",
		"soft off-white",
		"muted blue-grey",
		"dark neutral",
	}
)

// portraitDie — жребий камеры. Ключ — зерно и слуг жителя, поэтому один и тот
// же житель получает один и тот же кадр при каждом запуске: промпт сохраняют
// файлом и потом гоняют по нему картинку много раз, и «переснять» не должно
// означать «другой человек».
func portraitDie(seed uint64, slug string) *rand.Rand {
	return rand.New(rand.NewPCG(seed^portraitSalt, hashString(slug)))
}

func pick(rng *rand.Rand, from []string) string { return from[rng.IntN(len(from))] }

// ---------------------------------------------------------------- промпт

// Portrait собирает готовый английский промпт.
//
// look приходит от модели; пустой (когда её не звали — `-dry`) заменяется
// нейтральными оборотами, и промпт остаётся рабочим, просто безликим. Это не
// запасной путь на случай сбоя, а стенд: скелет видно, не потратив ни цента.
func Portrait(b Bio, slug string, seed uint64, look Look, midjourney bool) string {
	f := factsFor(b)
	rng := portraitDie(seed, slug)

	person := strings.TrimSpace(look.Person)
	if person == "" {
		person = "an ordinary, unremarkable face"
	}
	clothing := strings.TrimSpace(look.Clothing)
	if clothing == "" {
		clothing = "plain, well-worn everyday clothes"
	}

	var b1 strings.Builder
	// Первая строка говорит генератору главное: человека НЕТ. Это и защита от
	// сходства с реальным лицом, и просто правда.
	b1.WriteString("Photorealistic portrait photograph of a fictional person — not a real individual, not a celebrity.\n\n")

	fmt.Fprintf(&b1, "Subject: %s.\n", subjectLine(f))
	// Внешность — СВОЕЙ строкой с подписью, а не хвостом к Subject: клауза
	// модели сама начинается с «a tall man», и приклеенная давала
	// «...Russian man. a tall man...».
	fmt.Fprintf(&b1, "Build and face: %s.\n", trimDot(person))
	fmt.Fprintf(&b1, "Wearing %s.\n", trimDot(clothing))
	if d := strings.TrimSpace(look.Detail); d != "" {
		fmt.Fprintf(&b1, "%s.\n", trimDot(upperFirst(d)))
	}

	// Возраст на лице просим отдельной строкой и всегда: генераторы молодят и
	// прихорашивают, а тридцать красивых лиц подряд выдают машину раньше любой
	// стилистики.
	b1.WriteString("\nOrdinary-looking, not a model — natural skin texture with pores and blemishes, no retouching, no makeup styling.\n")
	fmt.Fprintf(&b1, "%s\n", agingLine(f.Age, f.Gender))

	b1.WriteString("\n")
	fmt.Fprintf(&b1, "Head and shoulders, %s, %s. %s. Plain %s background, softly out of focus.\n",
		pick(rng, portraitAngles), pick(rng, portraitFaces), pick(rng, portraitLight), pick(rng, portraitBack))
	b1.WriteString("Shot on a 50mm lens at f/2.8, available light, natural colour, sharp on the eyes.\n")

	// Кадр объявляем целью показа, а не пропорцией: аватар живёт квадратом
	// 100x100, и всё, что не лицо, на нём пропадает.
	b1.WriteString("\nSquare 1:1 composition, the face centred and filling most of the frame — the image will be shown as a 100x100 pixel avatar.\n")
	b1.WriteString("\nNo text, no letters, no logos, no watermark, no border, no collage, no second person, no hands in frame.\n")

	if midjourney {
		b1.WriteString("\n--ar 1:1 --style raw\n")
	}
	return b1.String()
}

// subjectLine — «a 45-year-old Russian man from a Siberian city». Возраст
// числом, а не полосой: полосу («middle-aged») генератор понимает шире, чем
// надо, и разброс по тридцати жителям смазывается.
func subjectLine(f portraitFacts) string {
	var s strings.Builder
	s.WriteString("a")
	if f.Age > 0 {
		fmt.Fprintf(&s, " %d-year-old", f.Age)
	}
	s.WriteString(" Russian ")
	switch f.Gender {
	case "female":
		s.WriteString("woman")
	case "male":
		s.WriteString("man")
	default:
		// Пол у жителя стоит всегда (его ставит enroll из карточки), но
		// молчаливо додумывать его здесь нельзя: мужчина «по умолчанию» назвал
		// бы мужчинами половину площадки — та же ошибка, из-за которой у
		// силуэта на странице заведён четвёртый, нейтральный.
		s.WriteString("person")
	}
	// Город идёт КЛИМАТОМ, а не видом за окном: фон у аватара размыт и пуст, а
	// сибирская зима видна на лице.
	if strings.TrimSpace(f.City) != "" {
		s.WriteString(" who lives in a Siberian city")
	}
	return s.String()
}

// agingLine — насколько лицу положено быть пожившим.
func agingLine(age int, gender string) string {
	// Местоимение берётся по полу, а не «his or her»: промпт читает генератор,
	// и развилка в тексте — лишний повод ему усомниться, кого рисовать.
	he, his := "They look", "their"
	switch gender {
	case "male":
		he, his = "He looks", "his"
	case "female":
		he, his = "She looks", "her"
	}
	switch {
	case age >= 55:
		return he + " " + his + " age: deep lines, grey hair, sagging skin under the eyes."
	case age >= 45:
		return he + " " + his + " age: visible lines, greying hair, tired eyes."
	case age >= 35:
		return he + " " + his + " age: the first lines around the eyes, skin that has seen weather."
	default:
		return "An everyday face, slightly uneven features."
	}
}

func trimDot(s string) string { return strings.TrimRight(strings.TrimSpace(s), ".") }
func upperFirst(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	r[0] = []rune(strings.ToUpper(string(r[0])))[0]
	return string(r)
}

// ------------------------------------------------------ запрос к модели

// portraitSystem — рамка. Запреты здесь ровно те, что нельзя доверить
// постпроцессору: чего в ответе быть не должно, того лучше и не порождать.
const portraitSystem = `You turn a short character sheet into three clauses of English text describing
how that person LOOKS. The result goes into an image-generation prompt for a
portrait photograph.

The character is FICTIONAL. Never name, reference or resemble any real or famous
person. Never invent a name.

Rules:
- Write only what is VISIBLE in a head-and-shoulders photograph. Not the job
  itself, not the biography, not the mood — what a stranger would see.
- The character sheet is in Russian; you answer in English.
- Ordinary people, not models. Give them uneven, forgettable, real faces:
  weight, thinning hair, bad haircuts, crooked teeth, weathered skin. This
  matters more than anything else — a beautiful face is a wrong answer.
- Let the job and the small facts show through the body and the clothes, not
  through props: a bus driver's shoulders and a hairdresser's own haircut, not
  a steering wheel and a pair of scissors.
- No text, logos, brands, uniforms with insignia, or writing of any kind.
- Plain noun phrases, no sentences, no "the image shows", no adjectives of
  praise. Each field at most 25 words.

Fields:
- person: build, face, hair, skin. Start with the body, end with the hair.
- clothing: everyday clothes this person would actually wear, plainly named.
- detail: ONE visible thing taken from the facts, if any of them is visible at
  all. If none is, return an empty string — do not invent one.`

// PortraitRequest — задание модели по одному жителю.
//
// Отдаёт СИСТЕМУ отдельно от задания, потому что система у тридцати жителей
// одна и та же: с кэш-точкой это тридцать раз по 0,1 цены вместо полной, и
// повтор здесь ДОКАЗАН устройством прогона, а не вероятен.
func PortraitRequest(b Bio) (system, prompt string) {
	f := factsFor(b)
	var p strings.Builder
	p.WriteString("Character sheet:\n")
	if f.Age > 0 {
		fmt.Fprintf(&p, "- age: %d\n", f.Age)
	}
	if f.Gender != "" {
		fmt.Fprintf(&p, "- gender: %s\n", f.Gender)
	}
	if f.City != "" {
		fmt.Fprintf(&p, "- city: %s\n", f.City)
	}
	if f.Job != "" {
		fmt.Fprintf(&p, "- work: %s\n", f.Job)
	}
	for _, x := range f.Facts {
		fmt.Fprintf(&p, "- fact: %s\n", x)
	}
	return portraitSystem, p.String()
}
