package morning

// «Доброе утро сегодня уже написал кто-то другой».
//
// Требование владельца, и оно про смысл фичи, а не про дубли в базе: утреннее
// приветствие в сообществе одно, и если его сказал человек — говорить второй
// раз должен не бот. Молчим и сообщаем в ЛС.
//
// Смотрим ЖИВУЮ ленту сайта, а не своё зеркало: зеркало отстаёт на такт обхода,
// а приветствие, написанное в 06:58, обязано нас остановить. Зеркало при этом
// нужно вторым вопросом — «а сегодняшняя ли это заметка»: лента отдаёт
// последние тридцать заметок без дат, и вчерашнее приветствие в ней ещё лежит.

import (
	"context"
	"strconv"
	"strings"
	"time"
	"unicode"

	"lovegw/internal/love"
)

// greetings — закрытый список зачинов. Список, а не регулярка с «утр.*»:
// «утренняя пробежка» и «с утра болит голова» — не приветствия, а мы на них
// промолчали бы весь день.
var greetings = []string{
	"доброе утро", "доброго утра", "с добрым утром", "утро доброе",
	"доброе утречко", "с добрым утречком", "всем утра", "всем доброго утра",
	"утречко", "утра доброго",
}

// greetWords — сколько первых СЛОВ тела считаем зачином. Слов, а не знаков:
// первая мерка была в 120 знаков, и на ней «Вчера поругались, а сегодня он
// сказал мне доброе утро…» — рассказ про ссору — засчитывался приветствием, то
// есть заставлял нас промолчать весь день. Шести слов хватает и на «Доброе
// утро», и на «Ну что, дорогие мои, доброе утро», а до середины фразы такое
// окно не достаёт.
const greetWords = 6

// isGreeting — начинается ли заметка с приветствия.
func isGreeting(text string) bool {
	words := strings.Fields(normalizeGreeting(text))
	if len(words) > greetWords {
		words = words[:greetWords]
	}
	head := strings.Join(words, " ")
	for _, g := range greetings {
		if strings.Contains(head, g) {
			return true
		}
	}
	return false
}

// normalizeGreeting — текст без регистра, знаков и «ё»: «Доброе Утро!!!», «с
// добрым утром)))» и «доброе утро, друзья» должны сходиться на одном образце.
func normalizeGreeting(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	space := false
	for _, r := range strings.ToLower(text) {
		switch {
		case r == 'ё':
			r = 'е'
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// foreignGreeting ищет в сегодняшней ленте чужое приветствие и возвращает id
// нашедшейся заметки ("" — никто не написал).
//
// «Сегодняшняя» решается по зеркалу: `first_seen_at` в границах суток. Заметки,
// которой в зеркале нет вовсе, верим на слово — она только что появилась, иначе
// обход давно бы её записал.
func (s *Service) foreignGreeting(ctx context.Context, feed []love.Note, start, end time.Time) string {
	for _, n := range feed {
		if n.AuthorID == s.cfg.OwnerProfileID || !isGreeting(n.Text) {
			continue
		}
		if s.seenToday(ctx, n.ID, start, end) {
			return n.ID
		}
	}
	return ""
}

func (s *Service) seenToday(ctx context.Context, noteID string, start, end time.Time) bool {
	known, err := s.st.NoteByID(ctx, noteID)
	if err != nil {
		return true // зеркало её ещё не видело — значит появилась только что
	}
	return !known.FirstSeenAt.Before(start) && known.FirstSeenAt.Before(end)
}

// stillInFeed — на месте ли наша вчерашняя заметка. Три исхода, и третий важен
// не меньше первых двух: лента отдаёт последние тридцать заметок, и заметка,
// уехавшая за нижний край, НЕ пропала — мы просто не можем о ней судить.
// Правило охвата то же, что у амвона и наблюдателя (`pageCovers`): считать
// пропажей можно лишь то, что страница вообще могла показать.
type presence int

const (
	presenceUnknown presence = iota // за краем окна — судить не по чему
	presenceThere                   // на месте
	presenceGone                    // окно её покрывает, а её нет
)

func notePresence(feed []love.Note, noteID string) presence {
	if noteID == "" || len(feed) == 0 {
		return presenceUnknown
	}
	oldest := feed[0].ID
	for _, n := range feed {
		if n.ID == noteID {
			return presenceThere
		}
		if lessID(n.ID, oldest) {
			oldest = n.ID
		}
	}
	if lessID(noteID, oldest) {
		return presenceUnknown // ушла за нижний край окна
	}
	return presenceGone
}

// lessID сравнивает id заметок сайта как числа: они монотонно растут по
// времени, и «старее» — это «меньше». Нечисловой id (такого у сайта не бывает)
// сравнивается лексикографически, лишь бы не паниковать.
func lessID(a, b string) bool {
	na, ea := strconv.ParseInt(a, 10, 64)
	nb, eb := strconv.ParseInt(b, 10, 64)
	if ea == nil && eb == nil {
		return na < nb
	}
	return a < b
}
