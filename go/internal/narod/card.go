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
	Roots     RootRate       `json:"roots"`
	Come      ComeRate       `json:"come"`
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

	// Tempo — та же мера по НАКАЛУ треда: сколько реплик прилетело за последние
	// минуты перед этой возможностью. Ею кубик узнаёт, что разговор разогрелся,
	// — а без неё вероятность отклика постоянна и в мёртвом треде, и в том, где
	// за минуту прилетело двадцать реплик. Ссора же явление ТЕМПОВОЕ: это
	// очередь ответов подряд между двумя, и при неизменной доле в проценты она не
	// заводится никогда (замер 28.08.2026, второй платный прогон).
	Tempo []RateBucket `json:"tempo,omitempty"`

	// ToMale/ToFemale — отклик по ПОЛУ говорящего. Замер, и крупный: разговор
	// структурно РАЗНОПОЛЫЙ. Наблюдённое против случайного выбора адресата —
	// мужчина женщине ×1,36, женщина мужчине ×1,31, женщина женщине ×0,82,
	// мужчина мужчине ×0,47 (вдвое реже случайного, и почти втрое реже, чем тот
	// же мужчина отвечает женщине; 300 тыс. рёбер, 29.08.2026).
	//
	// Кубик о поле адресата не знал ничего и выбирал вслепую. А величина эта
	// прямо про то, ради чего замер и делался: у живых сцепившаяся пара чаще
	// разнополая — значит и подтекст, и «мужчины с женщинами друг друга не
	// понимают» не украшение поверх ссоры, а её среда.
	ToMale   RateBucket `json:"to_male"`
	ToFemale RateBucket `json:"to_female"`
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

// FamiliarSpan — во сколько раз знакомство поднимает отклик в САМОМ сильном
// своём случае: замеренный размах рычага «кто говорит».
//
// Нужен он не сам по себе, а как ПОТОЛОК другому рычагу — отношению. Симпатию
// в архиве замерить нельзя вовсе: там видно, что человек написал, и не видно,
// как он к собеседнику относился. Но сказать «отношение решает не больше, чем
// решает знакомство» — можно, и это не догадка: знакомство и есть след того же
// самого («мы уже разговаривали, и я вернулся»), только доступный счёту.
//
// Отсюда разделение труда, которое и делает рычаг честным: РАЗМАХ берётся из
// замера, а модель называет одно лишь НАПРАВЛЕНИЕ и долю размаха. Придумано
// здесь ровно то, чего в архиве нет и быть не может, — знак.
func (r ReplyRate) FamiliarSpan(toHim bool) (float64, bool) {
	avg, ok := averageRate(r.Familiar, toHim)
	if !ok || avg <= 0 {
		return 1, false
	}
	best := avg
	for _, b := range r.Familiar {
		chances, answers := b.Chances, b.Answers
		if toHim {
			chances, answers = b.ToHimChances, b.ToHimAnswers
		}
		if chances < RateMinChances {
			continue
		}
		if got := float64(answers) / float64(chances); got > best {
			best = got
		}
	}
	return best / avg, true
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

// TempoRate — вероятность отклика при накале n (реплик за последние минуты).
// Второе значение — был ли это замер.
func (r ReplyRate) TempoRate(n int, toHim bool) (float64, bool) {
	return rateIn(r.Tempo, n, toHim)
}

// TempoLift — во сколько раз накал треда меняет отклик против среднего по всем
// накалам.
//
// Множителем, а не готовой вероятностью, по той же причине, что у знакомства:
// вопросы разные — «докуда дошёл разговор», «кто говорит» и «как сейчас
// горячо», — а три готовые вероятности содержат общую базу, и перемножить их
// значило бы учесть её трижды.
//
// Это и есть ОБРАТНАЯ СВЯЗЬ, которой кубику не хватало, — но не та, которую
// ожидали. Замер (см. archive.ReplyRate.Tempo) говорит, что в шумном треде
// человек влезает в ЧУЖОЙ разговор втрое реже, а отвечает обратившемуся к нему
// вдвое надёжнее. То есть горячий тред это не общий гвалт, а несколько
// параллельных диалогов, и перепалка растёт на ПАРЕ: я ответил тебе — ты почти
// наверняка ответишь мне.
//
// Разгон ограничен с двух сторон, и обе границы уже стоят: MaxChance у
// вероятности и MaxPerThread у числа реплик на человека, — то есть тред
// разогревается и затихает сам, а не уходит в шторм.
func (r ReplyRate) TempoLift(n int, toHim bool) (float64, bool) {
	got, ok := r.TempoRate(n, toHim)
	if !ok {
		return 1, false
	}
	avg, ok := averageRate(r.Tempo, toHim)
	if !ok || avg <= 0 {
		return 1, false
	}
	return got / avg, true
}

// RootRate — с какой вероятностью человек, УЖЕ говоривший в треде, зайдёт в
// саму заметку ещё раз, начав новую ветку, а не ответив кому-то.
//
// Замер, и заведён он под дыру в кубике, которую видно только по объёму треда
// (29.08.2026). «Прийти в заметку» бросалось РОВНО ОДИН РАЗ за её жизнь: не
// пришёл при публикации — не придёшь уже никогда, разве что ответом. При таком
// устройстве тридцать жителей не дадут больше двадцати девяти корней ни при
// какой вероятности, а в оригинале их 58 на заметку — и разрыв «79 реплик
// против 283» сидел ровно здесь: размер треда есть корни, делённые на
// (1 − разрастание).
//
// У живых повторный заход даёт 40 % ВСЕХ корней (10 конфликтных тредов, 583
// корня от 393 человек), а в позднем треде 2026 года — больше половины.
//
// Первый заход этой мерой не покрыт и покрыт быть не может: в архиве видно, в
// какую заметку человек пришёл, и не видно, какую пролистал, — знаменателя нет.
// У повторного он есть: человек уже в треде, и каждая чужая реплика после его
// первого слова — возможность, которой он либо воспользовался, либо нет. Та же
// единица, что у отклика, поэтому и корзины те же.
type RootRate struct {
	Threads int          `json:"threads"`
	Buckets []RateBucket `json:"buckets,omitempty"` // по позиции в треде
	Tempo   []RateBucket `json:"tempo,omitempty"`   // по накалу треда

	// Firsts/Repeats — корней первых и повторных в замеренных тредах. В кубик не
	// идут: это свидетельство о самом замере. Доля повторных у человека,
	// зашедшего всюду по разу, — ноль, и без этих двух чисел ноль читался бы как
	// «не любит начинать ветки», а не как «мерить было не на чем».
	Firsts  int `json:"firsts"`
	Repeats int `json:"repeats"`
}

// Rate — вероятность повторного захода на очередной чужой реплике в позиции pos.
// Второе значение — был ли это замер.
func (r RootRate) Rate(pos int) (float64, bool) { return rateIn(r.Buckets, pos, false) }

// TempoLift — во сколько раз накал n двигает повторный заход против среднего по
// замеру. Отдельным множителем, а не готовой вероятностью, по той же причине,
// что у отклика: позиция и накал описывают РАЗНОЕ («докуда дошёл разговор» и
// «как густо говорят прямо сейчас»), а обе готовые доли содержат общую базу,
// которая учлась бы дважды.
func (r RootRate) TempoLift(n int) (float64, bool) {
	got, ok := rateIn(r.Tempo, n, false)
	if !ok {
		return 1, false
	}
	avg, ok := averageRate(r.Tempo, false)
	if !ok || avg <= 0 {
		return 1, false
	}
	return got / avg, true
}

// ComeRate — с какой вероятностью человек, БЫВШИЙ на сайте, зайдёт в новую
// заметку сам, а не ответит кому-то в уже идущем разговоре.
//
// Замер, а не настройка, и до 29.08.2026 таковым не считался: у события будто бы
// нет знаменателя — видно, куда человек пришёл, и не видно, что он пролистал.
// Знаменатель находится, если спросить иначе: присутствие на сайте доказывается
// собственной репликой человека где угодно, и «был в тот день и в эту заметку не
// зашёл» — наблюдаемый промах. Живые заходят в 32 % заметок своего дня.
//
// Мера ЛИЧНАЯ, как разговорчивость: один открывает каждую вторую заметку, другой
// одну из двадцати, — и состав из «захожих» зажигает тред меньшим числом
// жителей.
//
// Верхняя оценка честно названа: день, когда человек читал и ничего не написал,
// в знаменатель не попадает, поэтому настоящая готовность не выше замеренной.
type ComeRate struct {
	Days    int `json:"days"`    // по скольким своим дням снято
	Chances int `json:"chances"` // заметок вышло в эти дни
	Came    int `json:"came"`    // в скольких он оказался корнем

	// LiveChances/LiveCame — то же по заметкам, У КОТОРЫХ РАЗГОВОР СОСТОЯЛСЯ, и
	// в кубик идут именно они. Знаменатель обязан совпадать с тем, к чему меру
	// прикладывают: по всем заметкам дня человек заходит в 14 %, но там в
	// знаменателе и мёртвые — те, где не написал никто; по живым он заходит в
	// 35 %. Наша заметка — та, ради которой всё затевалось, и назначать ей
	// заранее судьбу мёртвой значило бы мерить не то.
	LiveChances int `json:"live_chances"`
	LiveCame    int `json:"live_came"`
}

// ComeBase — доля ПРИСУТСТВУЮЩИХ на сайте, заходящих в заметку. Замер по 200
// живым заметкам последних лет: 35 %.
//
// Это и есть та величина, которую кубик задаёт на нашей площадке, потому что
// вопрос у него ровно такой: заметка вышла, житель на месте — зайдёт ли он.
const ComeBase = 0.35

// ComeTypical — типичная ЛИЧНАЯ доля: сколько заметок своего дня человек
// открывает. Замер по 90 донорам: среднее 0,141 (от 0,045 до 0,292).
//
// Стоит рядом с ComeBase не для красоты, а потому что две величины меряют одно
// поведение при РАЗНОЙ плотности заметок. В архивные годы их выходило от
// восьми до двадцати восьми в день, и внимание делилось между ними; у нашей
// площадки заметка одна. Поэтому личная доля берётся не сама по себе, а
// ОТНОШЕНИЕМ к типичной — так она остаётся тем, ради чего мерилась (один
// заходит вшестеро охотнее другого), и не тащит за собой чужую плотность.
const ComeTypical = 0.141

// PersonalShare — СЫРАЯ личная доля: сколько заметок своего дня человек
// открывал там, где его мерили. Второе значение — был ли это замер.
//
// Отдельно от Rate, и это не удобство: смешивать доноров и складывать замеры
// можно только сырыми долями. Композит, собранный из уже отмасштабированных,
// получает масштаб ВТОРОЙ раз — поймано ровно так 29.08.2026, когда у жителя
// личная доля вышла 50 % при донорских двадцати.
func (c ComeRate) PersonalShare() (float64, bool) {
	if c.LiveChances < RateMinChances {
		return 0, false
	}
	return float64(c.LiveCame) / float64(c.LiveChances), true
}

// Rate — готовность зайти в новую заметку НА НАШЕЙ ПЛОЩАДКЕ. Второе значение —
// был ли это замер: тощий счёт нельзя ни подставить в кубик, ни назвать нулём.
func (c ComeRate) Rate() (float64, bool) {
	personal, ok := c.PersonalShare()
	if !ok {
		return 0, false
	}
	return ComeBase * personal / ComeTypical, true
}

// AllRate — доля ВСЕХ заметок дня, включая мёртвые. В кубик не идёт, но стоит
// рядом: по паре чисел видно, насколько редка живая заметка вообще.
func (c ComeRate) AllRate() (float64, bool) {
	if c.Chances < RateMinChances {
		return 0, false
	}
	return float64(c.Came) / float64(c.Chances), true
}

// GenderLift — во сколько раз ПОЛ говорящего двигает готовность откликнуться
// против среднего по замеру. Второе значение — был ли это замер.
//
// Множителем, а не готовой долей, по той же причине, что у знакомства и накала:
// пол и позиция в треде описывают разное, а обе готовые доли содержат общую
// базу, которая учлась бы дважды. Неизвестный пол рычага не даёт вовсе —
// отсутствие наблюдения не есть третье состояние.
func (r ReplyRate) GenderLift(gender string, toHim bool) (float64, bool) {
	var b RateBucket
	switch gender {
	case "male":
		b = r.ToMale
	case "female":
		b = r.ToFemale
	default:
		return 1, false
	}
	got, ok := rateIn([]RateBucket{{Upto: 1 << 30, Chances: b.Chances, Answers: b.Answers,
		ToHimChances: b.ToHimChances, ToHimAnswers: b.ToHimAnswers}}, 0, toHim)
	if !ok {
		return 1, false
	}
	avg, ok := averageRate([]RateBucket{r.ToMale, r.ToFemale}, toHim)
	if !ok || avg <= 0 {
		return 1, false
	}
	return got / avg, true
}
