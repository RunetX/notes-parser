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
//   - СЛОЖЕНИЕ — тоже КОД, и по тому же доводу, только оплачен он боем
//     (30.08.2026, владелец: «почему они все какие-то упитанные»). Сперва его
//     выбирала модель, и у пятерых жителей из пяти вышло полное: в задании ей
//     стояло «дай настоящие лица: ВЕС, редеющие волосы, кривые зубы — это
//     важнее всего остального», и она исполнила список буквально. Ровно как
//     jabsLine с приёмами пассивной агрессии.
//
//   - ЛИЦО И ОДЕЖДА спрашиваются у МОДЕЛИ, и это не лень кода. «Рост под два
//     метра, отсюда и ник» и «красит волосы в новый цвет каждые два месяца» —
//     свободный русский текст, и превратить его в английскую строку про
//     внешность значит понять, что там сказано. Ровно та работа, ради которой
//     модель и зовут: там, где нужно суждение о СМЫСЛЕ, а не исполнение замера.
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
// И ОТДЕЛЬНО ПРО «ОБЫЧНОЕ ЛИЦО» — почему просьба об этом стоит ОДИН раз.
// Генераторы тянут к модельной внешности, и тридцать красивых лиц подряд выдают
// машину раньше любой стилистики. Но поправка против этого НЕ ЗНАЕТ МЕРЫ, и
// первая редакция промпта просила о ней ТРИЖДЫ — «не модель», «поры и пятна»,
// «без ретуши», — да ещё и клала сверху возраст односторонним «на лице должны
// быть годы». Просьбы сложились: у тридцатитрёхлетней воспитательницы вышло
// лицо на сорок пять (жалоба владельца, 30.08.2026).
//
// Отсюда правило, общее с односложностью реплик: у поправки называются ОБЕ
// стороны. Возраст теперь ЯКОРЬ — «ровно столько, не старше и не моложе», — а
// про обычность сказано единожды.

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
	// Face — ЛИЦО И ВОЛОСЫ, и только они: «a broad plain face with a crooked
	// nose and short grey hair». Сложения здесь нет намеренно — его бросает
	// жребий (portraitBuilds), потому что модель, которой доверили выбирать
	// сложение, выбрала одно и то же пятерым из пяти.
	Face string `json:"face"`
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
		"face":     map[string]any{"type": "string"},
		"clothing": map[string]any{"type": "string"},
		"detail":   map[string]any{"type": "string"},
	},
	"required":             []string{"face", "clothing", "detail"},
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
	// СЛОЖЕНИЕ — тоже жребий, и это правка от 30.08.2026 по жалобе владельца
	// («почему они все какие-то упитанные»). Раньше сложение выбирала МОДЕЛЬ, а
	// в задании ей стояло «дай им настоящие лица: ВЕС, редеющие волосы, кривые
	// зубы… это важнее всего остального» — и она послушалась буквально: у первых
	// же пяти жителей вышли «soft belly», «thickening waist», «heavy-set»,
	// «slight double chin», «slightly heavy build». Пять из пяти.
	//
	// Урок ровно тот, что записан в карте проекта дважды и который я тут
	// повторил: величина, живущая МЕЖДУ портретами, назначается ЖРЕБИЕМ, а не
	// просьбой. Список в промпте модель читает как рецепт и исполняет весь —
	// та же грабля, что у jabsLine с приёмами пассивной агрессии.
	//
	// Веса АВТОРСКИЕ, замера у них нет и быть не может: повторы в таблице и есть
	// вес. Полных двое из тринадцати — не потому, что столько в жизни, а потому
	// что тридцать одинаковых силуэтов выдают машину так же, как тридцать
	// одинаковых лиц.
	portraitBuilds = []string{
		"a thin build", "a thin build",
		"a lean build", "a lean build",
		"a wiry, slight build",
		"an average build", "an average build", "an average build",
		"an average build", "an average build",
		"a stocky, solid build", "a stocky, solid build",
		"a broad, heavy build",
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

	face := strings.TrimSpace(look.Face)
	if face == "" {
		face = "an ordinary, unremarkable face"
	}
	clothing := strings.TrimSpace(look.Clothing)
	if clothing == "" {
		clothing = "plain, well-worn everyday clothes"
	}

	var b1 strings.Builder
	// Первая строка говорит генератору главное: человека НЕТ. Это и защита от
	// сходства с реальным лицом, и просто правда.
	b1.WriteString("Photorealistic portrait photograph of a fictional person — not a real individual, not a celebrity.\n\n")

	// СЛОЖЕНИЕ — ЖРЕБИЙ, и стоит оно в той же строке, что возраст и пол: это
	// свойство человека, а не просьба к модели.
	fmt.Fprintf(&b1, "Subject: %s, %s.\n", subjectLine(f), pick(rng, portraitBuilds))
	// Лицо — СВОЕЙ строкой с подписью, а не хвостом к Subject: клауза модели
	// сама начинается с существительного, и приклеенная давала «...Russian man.
	// a tall man...».
	fmt.Fprintf(&b1, "Face and hair: %s.\n", trimDot(face))
	fmt.Fprintf(&b1, "Wearing %s.\n", trimDot(clothing))
	if d := strings.TrimSpace(look.Detail); d != "" {
		fmt.Fprintf(&b1, "%s.\n", trimDot(upperFirst(d)))
	}

	// Про обычность лица говорим РОВНО ОДИН РАЗ. Раньше здесь стояло три
	// синонимичных просьбы разом — «не модель», «поры и пятна», «без
	// ретуши», — и вместе с возрастной поправкой и словом «weight» в задании
	// модели они складывались: получалось не «обычный человек», а «потрёпанный».
	b1.WriteString("\nOrdinary rather than glamorous — a real face, not a model's.\n")
	fmt.Fprintf(&b1, "%s\n", ageAnchor(f.Age, f.Gender))

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

// ageAnchor — сколько лет лицу.
//
// ЯКОРЬ, А НЕ ТОЛЧОК, и переделано это 30.08.2026 по жалобе владельца: «даже
// Лисёнок, несмотря на юный возраст, выглядит на все 45». Стояло здесь
// одностороннее «клади на лицо возраст» — морщины, седина, усталые глаза, —
// заведённое из верного наблюдения, что генераторы молодят. Но поправка НЕ
// ЗНАЕТ МЕРЫ: возраст уже назван числом строкой выше, и вторая просьба про то
// же самое кладётся поверх, а не вместо. У тридцатитрёхлетней воспитательницы
// это дало «ruddy weathered skin» и лицо на сорок пять.
//
// Теперь названы ОБЕ стороны — «ровно столько, не старше и не моложе», — по
// тому же доводу, что у односложности реплик: односторонняя инструкция
// оставляет молчаливое умолчание, а оно сильнее просьбы. Возрастные приметы
// остаются только там, где они и есть у живых людей, и подаются скупо.
func ageAnchor(age int, gender string) string {
	// Местоимение берётся по полу, а не «his or her»: промпт читает генератор,
	// и развилка в тексте — лишний повод ему усомниться, кого рисовать.
	who := "They look"
	switch gender {
	case "male":
		who = "He looks"
	case "female":
		who = "She looks"
	}
	head := fmt.Sprintf("%s exactly %d — no older and no younger", who, age)
	switch {
	case age >= 55:
		return head + ": grey hair and clear lines, and nothing beyond that."
	case age >= 45:
		return head + ": light lines at the eyes, a little grey coming in."
	case age >= 35:
		return head + ": at most the first faint lines, smooth skin otherwise."
	default:
		return head + ": a young adult face, smooth skin, no ageing marks at all."
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
- BODY SIZE IS ALREADY DECIDED ELSEWHERE. Never mention weight, build, fat,
  thinness, a double chin, a belly, jowls, heavy cheeks or shoulders. Describe
  the FACE and the HAIR only. Height is the one exception, and only if the
  character sheet states it.
- AGE IS ALREADY STATED. Do not add ageing of your own: no wrinkles, no grey,
  no weathered or leathery skin, no tired or sunken eyes, unless the person is
  over fifty. A face that reads older than the stated age is a wrong answer.
- Ordinary rather than glamorous — a real face, not a model's. One or two plain
  imperfections are enough (an uneven nose, thin eyebrows, a crooked tooth, a
  bad haircut). Do not pile them up.
- Let the job and the small facts show through the hair and the clothes, not
  through props: a hairdresser's own haircut, not a pair of scissors.
- No text, logos, brands, uniforms with insignia, or writing of any kind.
- Plain noun phrases, no sentences, no "the image shows", no adjectives of
  praise. Each field at most 20 words.

Fields:
- face: the face and the hair, in that order. No body, no age marks.
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
