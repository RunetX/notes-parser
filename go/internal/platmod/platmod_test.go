package platmod

// Проверяется здесь ровно одно: ГРАНИЦА между мнением модели и решением
// площадки. Всё, что модель прислала, — это данные; что с ними делать, решает
// код, и именно эти правила ломаются тише всего.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"lovegw/internal/platform"
)

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// fakeStore — очередь в памяти. Ходить в Postgres ради проверки политики
// незачем: правила живут в Go, а SQL проверяют интеграционные тесты ядра.
type fakeStore struct {
	mu       sync.Mutex
	pending  []platform.Pending
	verdicts map[platform.Subject]platform.VerdictRecord
	attempts []platform.Subject
	same     int
	sameErr  error
	dirty    []int64
	settled  []int64
}

func newFakeStore(items ...platform.Pending) *fakeStore {
	return &fakeStore{pending: items, verdicts: map[platform.Subject]platform.VerdictRecord{}}
}

func (f *fakeStore) PendingChecks(_ context.Context, limit, _ int) ([]platform.Pending, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pending) > limit {
		return f.pending[:limit], nil
	}
	return f.pending, nil
}

func (f *fakeStore) BumpAttempts(_ context.Context, subs []platform.Subject) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts = append(f.attempts, subs...)
	return nil
}

func (f *fakeStore) RecordVerdict(_ context.Context, s platform.Subject, v platform.VerdictRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verdicts[s] = v
	return nil
}

func (f *fakeStore) SameBodyCount(_ context.Context, _ int64, _ string, _ time.Duration) (int, error) {
	return f.same, f.sameErr
}

func (f *fakeStore) DirtyVisibility(_ context.Context, _ int) ([]int64, error) {
	return f.dirty, nil
}

func (f *fakeStore) SettleVisibility(_ context.Context, id int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settled = append(f.settled, id)
	return true, nil
}

// fakeGen — модель, отвечающая заранее заготовленным JSON.
type fakeGen struct {
	reply  string
	err    error
	calls  int
	prompt string
}

func (g *fakeGen) GenerateJSON(_ context.Context, _, prompt string, _ map[string]any) ([]byte, error) {
	g.calls++
	g.prompt = prompt
	if g.err != nil {
		return nil, g.err
	}
	return []byte(g.reply), nil
}

func comment(id, author int64, body string) platform.Pending {
	return platform.Pending{
		Subject: platform.CommentSubject(id), NoteID: 5, AuthorID: author, Body: body,
	}
}

func answerJSON(items ...answerItem) string {
	b, err := json.Marshal(answer{Items: items})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// Главное правило Ш7: ссора — ЖАНР этого раздела, а не нарушение. Модель,
// назвавшая перепалку чистой, не должна ни во что упереться, а «уверенности»
// без цитаты недостаточно даже для той категории, которую автомат гасит сам.
func TestПолитикаВердиктов(t *testing.T) {
	cases := []struct {
		name string
		in   answerItem
		want platform.Verdict
		cat  string
	}{
		{"чисто", answerItem{Category: catClean, Reason: "ничего"}, platform.VerdictClean, ""},
		{"спам с цитатой", answerItem{Category: platform.CatSpam, Certain: true, Quote: "купи"},
			platform.VerdictHidden, platform.CatSpam},
		{"спам без уверенности", answerItem{Category: platform.CatSpam, Quote: "купи"},
			platform.VerdictReview, platform.CatSpam},
		{"спам без цитаты", answerItem{Category: platform.CatSpam, Certain: true},
			platform.VerdictReview, platform.CatSpam},
		{"сомнительное", answerItem{Category: platform.CatOther, Certain: true, Quote: "что-то"},
			platform.VerdictReview, platform.CatOther},
		{"выдуманная категория", answerItem{Category: "токсичность", Certain: true, Quote: "дурак"},
			platform.VerdictReview, platform.CatOther},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decide(c.in)
			if got.Verdict != c.want {
				t.Fatalf("вердикт %d, ожидался %d", got.Verdict, c.want)
			}
			if got.Category != c.cat {
				t.Fatalf("категория %q, ожидалась %q", got.Category, c.cat)
			}
		})
	}
}

// Категория, которой нет в закрытом списке автогашения, не гасится, сколько бы
// уверенности модель ни высказала. Это не перестраховка: список — и есть весь
// объём прав машины на чужие слова.
func TestГаситьМожноТолькоСписок(t *testing.T) {
	for _, cat := range platform.AutoCategories() {
		rec := decide(answerItem{Category: cat, Certain: true, Quote: "цитата"})
		want := platform.VerdictReview
		// Мат — единственное исключение, и оно в СТРОГУЮ сторону: гасит его код
		// по словарю корней, а мнение модели о брани зовёт человека (см. decide).
		if platform.AutoHideable(cat) && cat != platform.CatProfanity {
			want = platform.VerdictHidden
		}
		if rec.Verdict != want {
			t.Fatalf("категория %q: вердикт %d, ожидался %d", cat, rec.Verdict, want)
		}
	}
	if platform.AutoHideable(platform.CatOther) {
		t.Fatal("«на усмотрение модератора» не должно гаситься автоматом")
	}
}

// Мат гасит КОД, а мнение модели о брани зовёт человека. Разница не в тяжести,
// а в доказуемости: словарь корней воспроизводим и цитируется, «мне кажется,
// это брань» — нет. Прогон по живой очереди: из восьми «нецензурных» модели
// настоящим матом было одно.
func TestМатГаситСловарь_АМнениеМоделиЗовётЧеловека(t *testing.T) {
	st := newFakeStore(comment(1, 7, "Нахуя банить за флуд"), comment(2, 7, "хрен вас разберешь"))
	gen := &fakeGen{reply: answerJSON(
		answerItem{N: 1, Category: catClean},
		answerItem{N: 2, Category: platform.CatProfanity, Certain: true, Quote: "хрен"},
	)}
	s := New(Config{}, st, gen, quiet())

	if err := s.checkBatch(context.Background()); err != nil {
		t.Fatalf("такт: %v", err)
	}
	мат := st.verdicts[platform.CommentSubject(1)]
	if мат.Verdict != platform.VerdictHidden || мат.Category != platform.CatProfanity {
		t.Fatalf("настоящий мат: вердикт %d категория %q", мат.Verdict, мат.Category)
	}
	if мат.Quote == "" {
		t.Error("скрытие без цитаты: автору нечего показать")
	}
	грубость := st.verdicts[platform.CommentSubject(2)]
	if грубость.Verdict != platform.VerdictReview {
		t.Fatalf("грубость скрыта автоматом: вердикт %d", грубость.Verdict)
	}
}

// Стенд обязан показывать ровно то, что сделает бой. Пока словарь мата стоял
// прямо в такте, а Triage звал только модель, стенд «терял» настоящий мат — то
// есть предсказывал не то, что произойдёт, а это и есть единственная его работа.
func TestСтендРешаетТемЖе(t *testing.T) {
	items := []platform.Pending{comment(1, 7, "Нахуя банить за флуд")}
	gen := &fakeGen{reply: answerJSON(answerItem{N: 1, Category: catClean})}
	s := New(Config{}, newFakeStore(items...), gen, quiet())

	verdicts, err := s.Triage(context.Background(), items)
	if err != nil {
		t.Fatalf("стенд: %v", err)
	}
	if len(verdicts) != 1 || verdicts[0] == nil {
		t.Fatalf("стенд вернул %v", verdicts)
	}
	if verdicts[0].Verdict != platform.VerdictHidden || verdicts[0].Category != platform.CatProfanity {
		t.Fatalf("стенд не увидел мата: вердикт %d категория %q",
			verdicts[0].Verdict, verdicts[0].Category)
	}
}

// Отказ классификатора не должен ТЕРЯТЬ публикацию. Отказ приходит на всю
// пачку, попытки сгорают у всех, и строка, у которой они кончились, выпадает из
// очереди навсегда — не проверенная ни машиной, ни человеком. На живом прогоне
// так вышло с двадцатью строками из семисот тридцати восьми.
func TestНепроверенноеУходитЧеловеку(t *testing.T) {
	свежая := comment(1, 7, "первая попытка")
	последняя := comment(2, 7, "попытки кончились")
	последняя.Attempts = 2
	st := newFakeStore(свежая, последняя)
	gen := &fakeGen{err: errors.New("провайдер отклонил входной текст")}
	s := New(Config{MaxAttempts: 3}, st, gen, quiet())

	if err := s.checkBatch(context.Background()); err == nil {
		t.Fatal("отказ классификатора обязан вернуться ошибкой")
	}
	if _, ok := st.verdicts[platform.CommentSubject(1)]; ok {
		t.Error("строка с запасом попыток отдана человеку раньше времени")
	}
	rec, ok := st.verdicts[platform.CommentSubject(2)]
	if !ok {
		t.Fatal("исчерпавшая попытки строка потерялась молча")
	}
	if rec.Verdict != platform.VerdictReview || rec.Category != platform.CatOther {
		t.Fatalf("вердикт %d категория %q", rec.Verdict, rec.Category)
	}
	if rec.Quote != "" {
		t.Errorf("цитата взялась ниоткуда: %q", rec.Quote)
	}
}

// Шторм ловит КОД, а не модель: ей показывают одну реплику, и повтор она увидеть
// не может в принципе. Заодно это самый дешёвый способ погасить самое частое.
func TestШтормГаситсяБезМодели(t *testing.T) {
	st := newFakeStore(comment(1, 7, "одно и то же"))
	st.same = 5
	gen := &fakeGen{reply: answerJSON()}
	s := New(Config{}, st, gen, quiet())

	if err := s.checkBatch(context.Background()); err != nil {
		t.Fatalf("такт: %v", err)
	}
	rec, ok := st.verdicts[platform.CommentSubject(1)]
	if !ok {
		t.Fatal("вердикта нет")
	}
	if rec.Verdict != platform.VerdictHidden || rec.Category != platform.CatFlood {
		t.Fatalf("шторм: вердикт %d категория %q", rec.Verdict, rec.Category)
	}
	if gen.calls != 0 {
		t.Fatalf("модель звали %d раз — повтор ловится кодом", gen.calls)
	}
}

// Повтор текста у ЗАМЕТКИ штормом не считается: у заметок свой потолок частоты
// (одна в пять минут), и одинаковый текст там скорее правка через новую
// публикацию.
func TestПовторЗаметкиНеШторм(t *testing.T) {
	st := newFakeStore(platform.Pending{
		Subject: platform.NoteSubject(11), NoteID: 11, AuthorID: 7, Body: "текст",
	})
	st.same = 99
	gen := &fakeGen{reply: answerJSON(answerItem{N: 1, Category: catClean})}
	s := New(Config{}, st, gen, quiet())

	if err := s.checkBatch(context.Background()); err != nil {
		t.Fatalf("такт: %v", err)
	}
	if gen.calls != 1 {
		t.Fatalf("заметку не показали модели (звонков %d)", gen.calls)
	}
	if rec := st.verdicts[platform.NoteSubject(11)]; rec.Verdict != platform.VerdictClean {
		t.Fatalf("вердикт %d", rec.Verdict)
	}
}

// Пачка: модель отвечает про номера, и ответ обязан лечь на СВОИ строки.
// Промолчала про номер — строка остаётся в очереди, а не получает чужой вердикт.
func TestПачкаРаскладываетсяПоНомерам(t *testing.T) {
	st := newFakeStore(
		comment(1, 7, "первый"),
		comment(2, 8, "второй"),
		comment(3, 9, "третий"),
	)
	gen := &fakeGen{reply: answerJSON(
		answerItem{N: 3, Category: platform.CatSpam, Certain: true, Quote: "реклама"},
		answerItem{N: 1, Category: catClean},
	)}
	s := New(Config{Model: "тест"}, st, gen, quiet())

	if err := s.checkBatch(context.Background()); err != nil {
		t.Fatalf("такт: %v", err)
	}
	if got := st.verdicts[platform.CommentSubject(1)].Verdict; got != platform.VerdictClean {
		t.Fatalf("первый: вердикт %d", got)
	}
	if _, ok := st.verdicts[platform.CommentSubject(2)]; ok {
		t.Fatal("про второй модель промолчала — вердикта быть не должно")
	}
	if got := st.verdicts[platform.CommentSubject(3)].Verdict; got != platform.VerdictHidden {
		t.Fatalf("третий: вердикт %d", got)
	}
	if got := st.verdicts[platform.CommentSubject(3)].Model; got != "тест" {
		t.Fatalf("модель не записана: %q", got)
	}
	if len(st.verdicts[platform.CommentSubject(3)].PromptSHA) == 0 {
		t.Fatal("отпечаток промпта не записан — решение невоспроизводимо")
	}
	// Все три строки помечены попыткой ДО запроса: иначе строка, на которой
	// модель спотыкается, попадала бы в каждую пачку вечно.
	if len(st.attempts) != 3 {
		t.Fatalf("попыток отмечено %d, ожидалось 3", len(st.attempts))
	}
}

// Номер вне пачки модель присылать не должна, но если прислала — он молча
// пропускается, а не переписывает чью-то чужую строку.
func TestЧужойНомерИгнорируется(t *testing.T) {
	st := newFakeStore(comment(1, 7, "текст"))
	gen := &fakeGen{reply: answerJSON(
		answerItem{N: 42, Category: platform.CatSpam, Certain: true, Quote: "к"},
		answerItem{N: 0, Category: platform.CatSpam, Certain: true, Quote: "к"},
	)}
	s := New(Config{}, st, gen, quiet())

	if err := s.checkBatch(context.Background()); err != nil {
		t.Fatalf("такт: %v", err)
	}
	if len(st.verdicts) != 0 {
		t.Fatalf("проставлено %d вердиктов по чужим номерам", len(st.verdicts))
	}
}

// Отказ модели не должен ни ронять службу, ни оставлять половину пачки
// проверенной.
func TestОтказМоделиНеПортитОчередь(t *testing.T) {
	st := newFakeStore(comment(1, 7, "текст"))
	gen := &fakeGen{err: errors.New("сеть")}
	s := New(Config{}, st, gen, quiet())

	if err := s.checkBatch(context.Background()); err == nil {
		t.Fatal("отказ модели должен быть виден вызывающему")
	}
	if len(st.verdicts) != 0 {
		t.Fatal("при отказе модели вердиктов быть не должно")
	}
}

// Суточный потолок считает ЗАПРОСЫ — прямую единицу счёта денег.
func TestСуточныйПотолокЗапросов(t *testing.T) {
	st := newFakeStore(comment(1, 7, "текст"))
	gen := &fakeGen{reply: answerJSON(answerItem{N: 1, Category: catClean})}
	s := New(Config{DailyRequests: 2}, st, gen, quiet())

	ctx := context.Background()
	for range 5 {
		if err := s.checkBatch(ctx); err != nil {
			t.Fatalf("такт: %v", err)
		}
	}
	if gen.calls != 2 {
		t.Fatalf("запросов к модели %d, потолок 2", gen.calls)
	}
}

// Без классификатора служба обязана работать: очередь копится, модератор читает
// её глазами. Это рабочее состояние площадки, а не авария.
func TestБезКлассификатораОчередьЖива(t *testing.T) {
	st := newFakeStore(comment(1, 7, "текст"))
	s := New(Config{}, st, nil, quiet())

	if err := s.checkBatch(context.Background()); err != nil {
		t.Fatalf("такт: %v", err)
	}
	if len(st.verdicts) != 0 {
		t.Fatal("без модели вердиктов быть не может")
	}
	if len(st.attempts) != 0 {
		t.Fatal("попытки без запроса к модели не считаются")
	}
}

// Отложенные проходы по авторам доводятся независимо от классификатора: это
// исполнение отзыва согласия (ч. 2 ст. 9), а не работа модерации.
func TestОтложенныеПроходыДоводятся(t *testing.T) {
	st := newFakeStore()
	st.dirty = []int64{101, 102}
	s := New(Config{}, st, nil, quiet())

	s.tick(context.Background())
	if len(st.settled) != 2 {
		t.Fatalf("доведено проходов %d, ожидалось 2", len(st.settled))
	}
	// Второй такт подряд их не повторяет: расхождение появляется раз в недели,
	// и высматривать его каждые полминуты незачем.
	st.settled = nil
	s.tick(context.Background())
	if len(st.settled) != 0 {
		t.Fatalf("проходы повторены на соседнем такте: %v", st.settled)
	}
}

// Текст в промпте обрезается: заметка бывает на двадцать тысяч знаков, а
// решение про рекламу принимается по первым строкам.
func TestДлинныйТекстОбрезается(t *testing.T) {
	long := strings.Repeat("я", maxBodyRunes+500)
	st := newFakeStore(comment(1, 7, long))
	gen := &fakeGen{reply: answerJSON(answerItem{N: 1, Category: catClean})}
	s := New(Config{}, st, gen, quiet())

	if err := s.checkBatch(context.Background()); err != nil {
		t.Fatalf("такт: %v", err)
	}
	if !strings.Contains(gen.prompt, "[текст обрезан для проверки]") {
		t.Fatal("обрезка не отмечена в промпте — модель примет обрыв за оборванную мысль")
	}
	if n := len([]rune(gen.prompt)); n > maxBodyRunes+200 {
		t.Fatalf("в промпт уехало %d рун", n)
	}
}

// Схема ответа обязана перечислять ровно те категории, которые знает ядро:
// разъехавшись, промпт начнёт предлагать модели слова, за которыми у нас нет
// политики.
func TestСхемаСовпадаетСЯдром(t *testing.T) {
	schema := verdictSchema()
	items := schema["properties"].(map[string]any)["items"].(map[string]any)
	props := items["items"].(map[string]any)["properties"].(map[string]any)
	enum := props["category"].(map[string]any)["enum"].([]string)

	want := append(platform.AutoCategories(), catClean)
	if len(enum) != len(want) {
		t.Fatalf("в схеме %d категорий, в ядре %d", len(enum), len(want))
	}
	for i, c := range want {
		if enum[i] != c {
			t.Fatalf("категория %d: %q против %q", i, enum[i], c)
		}
	}
	for _, c := range platform.AutoCategories() {
		if !platform.KnownCategory(c) {
			t.Fatalf("категория %q не известна ядру", c)
		}
	}
	// Служебной «жалобы» в наборе автомата быть не должно: её ставит человек.
	if strings.Contains(fmt.Sprint(enum), platform.CatReport) {
		t.Fatal("«жалоба» попала в список категорий автомата")
	}
}
