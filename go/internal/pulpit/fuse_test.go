package pulpit

import (
	"strings"
	"testing"

	"lovegw/internal/store"
)

func TestFuseVerdict(t *testing.T) {
	cases := []struct {
		name string
		in   fuseInput
		off  bool
		says string
	}{
		{
			name: "две пропажи из трёх — рано",
			in:   fuseInput{Recent: []outcome{outcomeMissing, outcomeMissing}},
		},
		{
			name: "три подряд — выключаемся",
			in:   fuseInput{Recent: []outcome{outcomeMissing, outcomeMissing, outcomeMissing}},
			off:  true,
			says: "запрет",
		},
		{
			// 17.08.2026: сайт отвечал 500 на любой комментарий, включая чужие.
			// Три заметки подряд остались без реплики — и ни одного повода
			// заподозрить запрет писать.
			name: "5xx-шторм: не дошедшие отправки полосу не растят",
			in: fuseInput{
				Recent:  []outcome{outcomeNeutral, outcomeNeutral, outcomeNeutral},
				Profile: profileOK,
			},
		},
		{
			name: "исчезнувшая заметка полосу не рвёт и не растит",
			in: fuseInput{Recent: []outcome{
				outcomeMissing, outcomeVanished, outcomeMissing, outcomeVanished, outcomeMissing,
			}},
			off:  true,
			says: "запрет",
		},
		{
			name: "подтверждённая реплика рвёт полосу",
			in: fuseInput{Recent: []outcome{
				outcomeMissing, outcomeMissing, outcomeConfirmed, outcomeMissing, outcomeMissing,
			}},
		},
		{
			name: "две вычищенные реплики",
			in:   fuseInput{Recent: []outcome{outcomeDeleted, outcomeDeleted}},
			off:  true,
			says: "вычистили",
		},
		{
			name: "мёртвая сессия — сразу, без полосы",
			in:   fuseInput{Profile: profileUnauthorized},
			off:  true,
			says: "сессия",
		},
		{
			name: "закрытая анкета — сразу",
			in:   fuseInput{Profile: profileBlocked},
			off:  true,
			says: "анкета",
		},
		{
			name: "живая анкета при полосе: значит закрыт раздел",
			in: fuseInput{
				Recent:  []outcome{outcomeMissing, outcomeMissing, outcomeMissing},
				Profile: profileOK,
			},
			off:  true,
			says: "запрет",
		},
		{
			name: "пусто — работаем",
			in:   fuseInput{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			off, reason := fuseVerdict(tc.in, 3)
			if off != tc.off {
				t.Fatalf("выключение %v, ожидалось %v (причина %q)", off, tc.off, reason)
			}
			if tc.says != "" && !strings.Contains(reason, tc.says) {
				t.Errorf("причина %q не про «%s»", reason, tc.says)
			}
		})
	}
}

// TestOutcomeOf — как строки БД превращаются в исходы. Отправленная, но ещё не
// проверенная реплика промахом не считается: страницу мы не читали.
func TestOutcomeOf(t *testing.T) {
	cases := []struct {
		row  store.PulpitComment
		want outcome
	}{
		{store.PulpitComment{State: store.PulpitConfirmed}, outcomeConfirmed},
		{store.PulpitComment{State: store.PulpitMissing, Reason: reasonNoReply}, outcomeMissing},
		{store.PulpitComment{State: store.PulpitMissing, Reason: reasonDeleted}, outcomeDeleted},
		// POST не дошёл: тред читался, реплики в нём нет — и это ничего не
		// говорит о запрете писать.
		{store.PulpitComment{State: store.PulpitMissing, Reason: reasonSendFailed}, outcomeNeutral},
		{store.PulpitComment{State: store.PulpitVanished, Reason: reasonNoteGone}, outcomeVanished},
		{store.PulpitComment{State: store.PulpitPosted}, outcomeNeutral},
		{store.PulpitComment{State: store.PulpitSkipped, Reason: reasonColdStart}, outcomeNeutral},
		{store.PulpitComment{State: store.PulpitFailed, Reason: reasonNoLLM}, outcomeNeutral},
	}
	for _, tc := range cases {
		if got := outcomeOf(tc.row); got != tc.want {
			t.Errorf("%s/%s: исход %v, ожидался %v", tc.row.State, tc.row.Reason, got, tc.want)
		}
	}
}
