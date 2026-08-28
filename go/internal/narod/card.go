package narod

// Карточка персонажа — слой 1 брифа: всё, что о жителе известно ДО первой его
// реплики. Числа в ней замерены по архиву, а не выдуманы: «пиши коротко и с
// ошибками» модель понимает как карикатуру, а квантиль длины и частота ошибки на
// тысячу слов — это цель, которую можно проверить.
//
// Карточек два сорта, и различие между ними не техническое, а правовое:
//
//   - СЛЕПОК (KindSnapshot) — портрет реального участника архива вместе с его
//     ником и образцами его писем. Живёт только в офлайн-калибровке: реплеем
//     поднимается старая заметка, слепки переигрывают тред, и результат сверяется
//     с оригиналом. Наружу слепок не выходит никогда — это была бы имперсонация
//     живого человека на открытой площадке.
//   - КОМПОЗИТ (KindComposite) — житель площадки. Числовые цели смешаны из
//     нескольких доноров, дословных образцов нет вовсе, а ник и биографию пишет
//     владелец. Похож на манеру, но не на человека.
//
// Правило «в live выходят только композиты» живёт ДВАЖДЫ — в CheckLive и в
// проверке конфигурации, — потому что цена ошибки здесь не поломка, а публикация.
//
// Масштабирование до десяти персонажей — это положить в каталог ещё файлы:
// список жителей нигде в коде не перечислен (DoD брифа).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Сорта карточек.
const (
	KindSnapshot  = "snapshot"  // слепок реального участника: только калибровка
	KindComposite = "composite" // житель площадки: смесь доноров, своя биография
)

// CardExt — расширение файлов каталога. Формат JSON, а не YAML: карта письма из
// архива приезжает JSON'ом, и второй сериализатор ради удобства правки руками
// стоил бы расхождения форматов.
const CardExt = ".json"

// MinNickRunes — короче ник заводить нельзя. Причина не в красоте: площадка
// раздаёт поводы «вас упомянули», разбирая тело реплики на слова длиной от трёх
// (platform.fanOutRules), и житель по имени «Ку» получал бы повод от каждого
// случайного слога.
const MinNickRunes = 3

// Dist — квантильная сводка. Медиана и края важнее среднего: длины реплик
// длиннохвостые, и одна простыня уводит среднее туда, где человек не пишет
// никогда.
type Dist struct {
	P10    int `json:"p10"`
	Median int `json:"median"`
	P90    int `json:"p90"`
	Max    int `json:"max"`
}

// Count — «сколько раз встретилось» для топ-списков (смайлы, зачины).
type Count struct {
	Text  string  `json:"text"`
	Share float64 `json:"share"` // доля реплик, где встречается
}

// Bio — то, что персонаж о себе знает. Кроме ника, площадка этих полей не
// хранит нигде: анкеты у неё нет, а противоречить себе персонаж не вправе —
// поэтому факты всегда едут в промпт и механически сверяются с готовым текстом.
type Bio struct {
	Nick   string   `json:"nick"`
	Gender string   `json:"gender,omitempty"` // male | female | "" — не назван
	Age    int      `json:"age,omitempty"`
	City   string   `json:"city,omitempty"`
	Job    string   `json:"job,omitempty"`
	Family string   `json:"family,omitempty"`
	Facts  []string `json:"facts,omitempty"` // жёсткие факты: с ними сверяется реплика
}

// Register — механика письма: как человек ставит знаки, а не что говорит.
// Поля повторяют замеры archive.VoiceShape по смыслу, но не по устройству —
// см. шапку пакета о том, почему тип свой.
type Register struct {
	Runes      Dist    `json:"runes"`        // длина реплики
	SentWords  Dist    `json:"sent_words"`   // длина предложения в словах
	SentWordSD float64 `json:"sent_word_sd"` // разброс длин — сильнейший признак живого письма
	ShortSents float64 `json:"short_sents"`  // доля предложений ≤3 слов
	LongSents  float64 `json:"long_sents"`   // доля предложений ≥18 слов

	Punct     map[string]float64 `json:"punct,omitempty"`      // знаков на 1000 рун
	ParenRuns map[string]float64 `json:"paren_runs,omitempty"` // «)» против «))» — разные люди

	Smileys    []Count  `json:"smileys,omitempty"` // коды :::…::: самого сайта
	SmileyRate float64  `json:"smiley_rate"`       // смайлов на реплику
	EmojiRate  float64  `json:"emoji_rate"`        // 0 — эмодзи персонажу запрещены
	Emoji      []string `json:"emoji,omitempty"`   // и какие именно он ставит

	AllLower     float64 `json:"all_lower"`      // доля реплик целиком строчными
	StartsLower  float64 `json:"starts_lower"`   // доля реплик со строчной буквы
	NoFinalPunct float64 `json:"no_final_punct"` // доля реплик без знака в конце
	YoRate       float64 `json:"yo_rate"`        // «ё» на 1000 рун

	Openings      []Count  `json:"openings,omitempty"`  // чем начинает
	AddressPrefix float64  `json:"address_prefix"`      // доля реплик «Ник, …»
	Parasites     []string `json:"parasites,omitempty"` // слова-приклейки
}

// Word — характерное слово из словаря автора (вес tf·idf у донора).
type Word struct {
	Word  string  `json:"word"`
	TFIDF float64 `json:"tfidf"`
}

// Sample — дословный образец письма. Есть ТОЛЬКО у слепка: перенести образцы в
// композит значило бы перенести в него чужие фразы, а с ними и человека.
type Sample struct {
	Text          string `json:"text"`
	Context       string `json:"context,omitempty"`        // на что отвечает
	ContextAuthor string `json:"context_author,omitempty"` // и чьи это слова
}

// VariantErrorID — общий id личных словоформ («вобщем» вместо «в общем»): класс
// у них один, а различает их пара «как правильно → как пишет он». Значение
// совпадает с archive.VariantErrorID, и это стережёт парный тест: ядро
// эмуляции архив не импортирует.
const VariantErrorID = "variant"

// ErrorPattern — характерная ошибка как ЧИСЛОВАЯ ЦЕЛЬ, а не как просьба к
// модели. Просьбу «пиши с ошибками» модель исполняет карикатурой и вразнобой;
// здесь же ошибку вносит детерминированный постпроцессор с заданной частотой,
// поэтому у персонажа она всегда одна и та же — как у человека.
type ErrorPattern struct {
	ID      string            `json:"id"`   // какой класс ошибки
	Rate    float64           `json:"rate"` // на 1000 слов
	Params  map[string]string `json:"params,omitempty"`
	Norm    string            `json:"norm,omitempty"`    // как правильно
	Variant string            `json:"variant,omitempty"` // как пишет он
}

// LatencyDist — через сколько человек приходит. Без этого замера все жители
// сбегаются в тред за одну минуту, и это первое, что выдаёт машину.
type LatencyDist struct {
	ToThreadSec Dist `json:"to_thread_sec"` // от заметки до первой своей реплики
	ToReplySec  Dist `json:"to_reply_sec"`  // от чужой реплики до ответа на неё
}

// Rhythm — когда персонаж вообще бывает на площадке. Часы в поясе площадки
// (Новосибирск): реплика в четыре утра от человека, который никогда не писал
// ночью, — это не разнообразие, а поломка.
type Rhythm struct {
	TZ       string  `json:"tz"`
	Hours    [24]int `json:"hours"`
	Weekdays [7]int  `json:"weekdays"`
}

// Topic — что персонажа цепляет, а что оставляет равнодушным. Отрицательный
// вес — законный вход для «промолчать»: молчание в брифе обязательный исход,
// а не отказ службы.
type Topic struct {
	Name   string  `json:"name"`
	Weight float64 `json:"weight"` // -1 … +1

	// Key — ключ лексикона темы (cats, cars, dacha…). По нему тема сходится с
	// заметкой; Name — то же самое по-русски, и живёт оно ради брифа, который
	// читают человек и модель.
	Key string `json:"key,omitempty"`

	// Lift — ЗАМЕР: во сколько раз чаще человек заходил в заметку на эту тему,
	// чем такие заметки встречались в его время. Ноль значит «не мерили» — не
	// «не интересуется», и путать эти два состояния нельзя. Вес рядом остаётся:
	// он про то, КАК человек про тему говорит (любит или морщится), а перекос —
	// про то, ЗАЙДЁТ ли он вообще.
	Lift float64 `json:"lift,omitempty"`
}

// SeedEdge — стартовое отношение. В карточке только НАЧАЛЬНОЕ значение: дальше
// отношения живут в состоянии мира и меняются после каждого треда, иначе
// эмуляции не выйдет вовсе.
type SeedEdge struct {
	Actor    string  `json:"actor"`    // id персонажа в каталоге
	Sympathy float64 `json:"sympathy"` // -10 … +10
	Note     string  `json:"note,omitempty"`
}

// DiceParams — вероятности прихода. Базовая доля намеренно невысока: по брифу
// четверо-шестеро из десяти не приходят вовсе, и это свойство мира, а не сбой.
type DiceParams struct {
	ComeToNote   float64 `json:"come_to_note"`   // базовая вероятность прийти в новую заметку
	ReplyMention float64 `json:"reply_mention"`  // ответить, когда обратились к нему
	ReplyOther   float64 `json:"reply_other"`    // влезть в чужой разговор
	MaxPerThread int     `json:"max_per_thread"` // сколько реплик за тред
	MaxPerDay    int     `json:"max_per_day"`
}

// Card — карточка персонажа целиком.
type Card struct {
	Stamp   Stamp    `json:"stamp"`
	ID      string   `json:"id"`   // slug: стабильный ключ актора в мире
	Kind    string   `json:"kind"` // snapshot | composite
	Sources []string `json:"sources,omitempty"`

	// Accounts — номера анкет, из которых снят слепок. У человека с альтами их
	// несколько, и карточка тогда ложится под номером ЛИЧНОСТИ (p<id>), а не
	// анкеты: `narod card u1496130` пишет файл `p1713.json`. Без этого списка
	// найти карточку по анкете, под которой человек говорил в архивном треде,
	// было бы нечем — а реплей знает его именно по ней.
	//
	// У композита пуст: там доноров несколько и смешаны они числами, так что
	// «под какой анкетой он говорил» вопрос без ответа.
	Accounts []int64 `json:"accounts,omitempty"`

	Persona   Bio            `json:"persona"`
	Register  Register       `json:"register"`
	Vocab     []Word         `json:"vocab,omitempty"`
	VocabRate float64        `json:"vocab_rate"` // слов из словаря на 100 слов у самого автора
	Samples   []Sample       `json:"samples,omitempty"`
	Errors    []ErrorPattern `json:"errors,omitempty"`
	Latency   LatencyDist    `json:"latency"`
	Rhythm    Rhythm         `json:"rhythm"`
	Triggers  []Topic        `json:"triggers,omitempty"`
	Relations []SeedEdge     `json:"relations,omitempty"`
	Rate      ReplyRate      `json:"rate"`
	Dice      DiceParams     `json:"dice"`
	Seed      int64          `json:"seed"`
}

// slugRe — id карточки: он же ключ актора в мире и часть имени файла.
var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,31}$`)

// Validate проверяет карточку на пригодность к работе вообще — независимо от
// того, в каком режиме она будет играть. Причина возвращается словами: её
// читает человек, собравший карточку руками.
func (c Card) Validate() error {
	if !slugRe.MatchString(c.ID) {
		return fmt.Errorf("id %q: латиница, цифры и дефис, 2–32 знака", c.ID)
	}
	if c.Kind != KindSnapshot && c.Kind != KindComposite {
		return fmt.Errorf("%s: неизвестный сорт карточки %q (%s|%s)", c.ID, c.Kind, KindSnapshot, KindComposite)
	}
	nick := strings.TrimSpace(c.Persona.Nick)
	if nick == "" {
		return fmt.Errorf("%s: без ника — под ним подписывать реплики", c.ID)
	}
	if nick != c.Persona.Nick {
		return fmt.Errorf("%s: ник %q с краевыми пробелами", c.ID, c.Persona.Nick)
	}
	// Требование к длине — про ПЛОЩАДКУ, а не про письмо: она раздаёт поводы
	// «вас упомянули», разбирая тело реплики на слова от трёх знаков, и житель
	// по имени «ДВ» получал бы повод от каждого случайного слога. К слепку это
	// правило не относится: у него анкеты нет и не будет, а ник у него не наш —
	// он такой, каким был у человека («ДВ» из архива, замер 27.08.2026).
	if c.Kind == KindComposite && utf8.RuneCountInString(nick) < MinNickRunes {
		return fmt.Errorf("%s: ник %q короче %d знаков — площадка раздаёт по нему поводы «вас упомянули»",
			c.ID, c.Persona.Nick, MinNickRunes)
	}
	if c.Register.Runes.Median <= 0 {
		return fmt.Errorf("%s: не замерена длина реплики — карточка собрана не майнером", c.ID)
	}
	if c.Register.Runes.P90 > 0 && c.Register.Runes.P10 > c.Register.Runes.P90 {
		return fmt.Errorf("%s: квантили длины перепутаны (p10 %d > p90 %d)",
			c.ID, c.Register.Runes.P10, c.Register.Runes.P90)
	}
	if c.Dice.ComeToNote < 0 || c.Dice.ComeToNote > 1 ||
		c.Dice.ReplyMention < 0 || c.Dice.ReplyMention > 1 ||
		c.Dice.ReplyOther < 0 || c.Dice.ReplyOther > 1 {
		return fmt.Errorf("%s: вероятности прихода вне [0,1]", c.ID)
	}
	if c.Kind == KindComposite && len(c.Samples) > 0 {
		return fmt.Errorf("%s: у композита есть дословные образцы донора — это и есть та утечка, ради которой он заводился", c.ID)
	}
	for _, e := range c.Errors {
		if e.Rate < 0 {
			return fmt.Errorf("%s: ошибка %q с отрицательной частотой", c.ID, e.ID)
		}
	}
	return nil
}

// CheckLive решает, годится ли набор карточек для публикации на площадке.
//
// Второе место, где записано «наружу выходят только композиты» (первое — сборка
// конфигурации, отказывающая службе на старте). Дублирование намеренное: между
// двумя проверками лежит вся отладка, и ошибиться здесь значит опубликовать под
// чужой манерой письма реплику, которую тот человек не писал.
func CheckLive(cards []Card) error {
	if len(cards) == 0 {
		return fmt.Errorf("каталог пуст: играть некому")
	}
	for _, c := range cards {
		if c.Kind != KindComposite {
			return fmt.Errorf("карточка %s — %s: наружу выходят только композиты, слепок живёт в калибровке", c.ID, c.Kind)
		}
	}
	return nil
}

// LoadCard читает одну карточку.
func LoadCard(path string) (Card, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Card{}, err
	}
	var c Card
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields() // опечатка в имени поля молча обнулила бы замер
	if err := dec.Decode(&c); err != nil {
		return Card{}, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if err := c.Validate(); err != nil {
		return Card{}, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return c, nil
}

// LoadCards читает каталог персонажей.
//
// Порядок — по имени файла, и это не педантизм: от порядка зависят броски
// кубика в тесте, а тест, зависящий от порядка обхода каталога, врёт через раз.
// Каталог, в котором нет ни одной карточки, — ошибка: молча промолчавшая
// служба неотличима от сломанной.
func LoadCards(dir string) ([]Card, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("каталог персонажей %s: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), CardExt) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("каталог персонажей %s: ни одной карточки %s", dir, CardExt)
	}

	cards := make([]Card, 0, len(names))
	seen := make(map[string]string, len(names))
	nicks := make(map[string]string, len(names))
	for _, name := range names {
		c, err := LoadCard(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[c.ID]; dup {
			return nil, fmt.Errorf("%s: id %s уже занят карточкой %s", name, c.ID, prev)
		}
		// Ник — подпись под репликой и адрес обращения «Ник, …»: два жителя с
		// одним ником сделали бы неразрешимой саму адресацию в треде.
		if prev, dup := nicks[strings.ToLower(c.Persona.Nick)]; dup {
			return nil, fmt.Errorf("%s: ник %q уже занят карточкой %s", name, c.Persona.Nick, prev)
		}
		seen[c.ID], nicks[strings.ToLower(c.Persona.Nick)] = name, name
		cards = append(cards, c)
	}
	return cards, nil
}

// Nicks — ники каталога. Нужны стоп-листу имён: имя, не принадлежащее ни
// жителю, ни треду, в реплике запрещено (урок инцидента, когда модель назвала
// человека случайным именем).
func Nicks(cards []Card) []string {
	out := make([]string, 0, len(cards))
	for _, c := range cards {
		out = append(out, c.Persona.Nick)
	}
	return out
}

// ReplyRate — с какой вероятностью человек откликается на очередную реплику, по
// позиции той в треде. Замер, а не настройка: кубик, у которого вероятность
// одна на весь тред, одинаково рвётся в беседу на двадцатой реплике и на
// трёхсотой (замер 28.08.2026 — 71 приход мимо на тред в 298 реплик).
//
// Две меры порознь, потому что «к нему обратились» и «мимо него говорят»
// отличаются на порядок: одна средняя между ними не описывает ни того, ни
// другого.
type ReplyRate struct {
	Threads int          `json:"threads"`
	Buckets []RateBucket `json:"buckets,omitempty"`

	// Familiar — та же мера по ЗНАКОМСТВУ: сколько раз человек уже отвечал
	// говорящему. Ею проверяется, вправе ли граф отношений двигать решение и
	// насколько; без замера множитель за «своего» был бы такой же выдумкой, как
	// прежнее «влезть в чужой разговор = 0.15».
	Familiar []RateBucket `json:"familiar,omitempty"`
}

// FamiliarRate — вероятность отклика на реплику того, кому уже отвечал prior
// раз. Второе значение — был ли это замер.
func (r ReplyRate) FamiliarRate(prior int, toHim bool) (float64, bool) {
	return rateIn(r.Familiar, prior, toHim)
}

// FamiliarLift — во сколько раз знакомство меняет вероятность отклика против
// среднего по всем знакомствам.
//
// Множителем, а не готовой вероятностью, потому что вопросов два и они разные:
// позиция в треде говорит «докуда дошёл разговор», знакомство — «кто говорит».
// Перемножить две готовые вероятности нельзя (обе содержат общую базу и она
// учлась бы дважды), поэтому вторая берётся ОТНОСИТЕЛЬНО собственного среднего.
//
// Замер 28.08.2026 по трём слепкам: у реплики, обращённой мимо человека,
// знакомство поднимает отклик в 2,8–4,7 раза (у ДВ с 0,20 % до 0,95 %), а у
// реплики, обращённой К НЕМУ, не меняет почти ничего (54→59 %, 57→45 %,
// 57→61 %). То есть знакомство решает, влезешь ли ты в ЧУЖОЙ разговор, и не
// решает ничего, когда обратились лично к тебе. Множитель, наложенный на оба
// случая разом, был бы выдумкой ровно там, где её видно.
func (r ReplyRate) FamiliarLift(prior int, toHim bool) (float64, bool) {
	got, ok := r.FamiliarRate(prior, toHim)
	if !ok {
		return 1, false
	}
	avg, ok := averageRate(r.Familiar, toHim)
	if !ok || avg <= 0 {
		return 1, false
	}
	return got / avg, true
}

// averageRate — отклик по всем корзинам разом: знаменатель множителя.
func averageRate(buckets []RateBucket, toHim bool) (float64, bool) {
	var chances, answers int
	for _, b := range buckets {
		if toHim {
			chances, answers = chances+b.ToHimChances, answers+b.ToHimAnswers
			continue
		}
		chances, answers = chances+b.Chances, answers+b.Answers
	}
	if chances < RateMinChances {
		return 0, false
	}
	return float64(answers) / float64(chances), true
}

// RateBucket — отклик в одной корзине позиций. Хранятся ЧИСЛА, а не готовая
// доля: по числу возможностей видно, замер это или три случая.
type RateBucket struct {
	Upto         int `json:"upto"`
	Chances      int `json:"chances"`
	Answers      int `json:"answers"`
	ToHimChances int `json:"to_him_chances"`
	ToHimAnswers int `json:"to_him_answers"`
}

// RateMinChances — ниже этого корзина замером не считается.
const RateMinChances = 30

// Rate — вероятность отклика на реплику в позиции pos. Второе значение — был ли
// это замер: пустую корзину нельзя ни подставить в кубик, ни назвать нулём.
func (r ReplyRate) Rate(pos int, toHim bool) (float64, bool) {
	return rateIn(r.Buckets, pos, toHim)
}

func rateIn(buckets []RateBucket, x int, toHim bool) (float64, bool) {
	for _, b := range buckets {
		if x > b.Upto {
			continue
		}
		chances, answers := b.Chances, b.Answers
		if toHim {
			chances, answers = b.ToHimChances, b.ToHimAnswers
		}
		if chances < RateMinChances {
			return 0, false
		}
		return float64(answers) / float64(chances), true
	}
	return 0, false
}

// TopicLift — множитель к «прийти в заметку» по её темам.
//
// Берётся МАКСИМУМ, а не среднее: заметка, где человеку интересна хоть одна
// тема, тянет его туда целиком — а среднее гасило бы этот интерес темами, к
// которым он равнодушен, и заметка «про машины и про дачу» звала бы автолюбителя
// слабее, чем просто «про машины».
//
// Неизмеренные темы пропускаются, а не считаются единицей: у них ноль в Lift, и
// подставить его значило бы объявить человеку отвращение к теме, про которую
// замера просто не было. Второе значение — нашлась ли хоть одна измеренная: без
// неё множителя нет вовсе, и звать это «единицей» тоже нельзя.
func (c Card) TopicLift(keys []string) (float64, bool) {
	best, ok := 0.0, false
	for _, key := range keys {
		for _, t := range c.Triggers {
			if t.Key != key || t.Lift <= 0 {
				continue
			}
			if t.Lift > best {
				best, ok = t.Lift, true
			}
		}
	}
	return best, ok
}

// hourLocs — часовые пояса карточек. Их единицы, а time.LoadLocation читает
// файл; кубик же зовут десятки тысяч раз за прогон.
var hourLocs sync.Map

// HourWeight — во сколько раз человек деятельнее обычного в этот час.
//
// Считается прямо из замера ритма и нормируется на среднее, поэтому среднее по
// суткам равно единице: множитель ПЕРЕРАСПРЕДЕЛЯЕТ приходы по часам, а не
// добавляет и не убавляет их. Это важно — базовая вероятность прийти в заметку
// не измерима (в архиве видно, куда человек пришёл, и не видно, что он
// пролистал), и множитель, сдвигающий её среднее, менял бы неизвестное число
// неизвестно куда.
//
// Час берётся В ПОЯСЕ ЧЕЛОВЕКА: у части архивных доноров он не новосибирский
// (замеры досье находили и +3, и +5), и ночной пик москвича в новосибирских
// часах выглядел бы дневным.
//
// Пустой ритм даёт единицу — «не мерили», а не «никогда не бывает».
func (r Rhythm) HourWeight(t time.Time) float64 {
	sum := 0
	for _, n := range r.Hours {
		sum += n
	}
	if sum == 0 {
		return 1
	}
	h := t.In(r.Location()).Hour()
	return float64(r.Hours[h]) * 24 / float64(sum)
}

// Location — пояс ритма; неизвестный или пустой означает UTC.
func (r Rhythm) Location() *time.Location {
	if r.TZ == "" {
		return time.UTC
	}
	if v, ok := hourLocs.Load(r.TZ); ok {
		return v.(*time.Location)
	}
	loc, err := time.LoadLocation(r.TZ)
	if err != nil {
		loc = time.UTC
	}
	hourLocs.Store(r.TZ, loc)
	return loc
}
