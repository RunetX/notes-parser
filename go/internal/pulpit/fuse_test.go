package pulpit

import (
	"strings"
	"testing"
	"time"

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

// Второй дефект того же выключения 23.08.2026: у полосы не было горизонта.
//
// Строк амвон пишет мало, и почти все — пропуски холодного старта, нейтральные
// для счёта. Поэтому тридцать последних строк оказались НЕДЕЛЕЙ С ЛИШНИМ: одна
// вычищенная реплика от 15.08 и вторая от 23.08 сложились в «чистку», хотя между
// ними восемь дней. Разорвать полосу могла бы только подтверждённая реплика, а
// её за эти дни не было — амвон почти всё время молчал.
func TestFuseIgnoresDeletionsOlderThanHorizon(t *testing.T) {
	now := time.Now()
	rows := []store.PulpitComment{
		// от новых к старым, как отдаёт PulpitRecent
		{NoteID: "313058", State: store.PulpitMissing, Reason: reasonDeleted, PostedAt: now.Add(-2 * time.Hour)},
		{NoteID: "313057", State: store.PulpitSkipped, Reason: "cold_start"},
		{NoteID: "312999", State: store.PulpitMissing, Reason: reasonDeleted, PostedAt: now.Add(-8 * 24 * time.Hour)},
	}

	kept := withinHorizon(rows, now)
	outcomes := make([]outcome, 0, len(kept))
	for _, row := range kept {
		outcomes = append(outcomes, outcomeOf(row))
	}
	_, deleted := streakOf(outcomes)
	if deleted != 1 {
		t.Fatalf("в полосе %d вычищенных, ожидалась одна: восьмидневной давности не считается", deleted)
	}
	off, reason := fuseVerdict(fuseInput{Recent: outcomes, Profile: profileOK}, 3)
	if off {
		t.Fatalf("предохранитель сработал на одной чистке: %q", reason)
	}
}

// А две чистки ВНУТРИ горизонта по-прежнему гасят фичу: горизонт сужает окно, а
// не отменяет правило.
func TestFuseStillTripsOnTwoRecentDeletions(t *testing.T) {
	now := time.Now()
	rows := []store.PulpitComment{
		{NoteID: "n2", State: store.PulpitMissing, Reason: reasonDeleted, PostedAt: now.Add(-2 * time.Hour)},
		{NoteID: "n1", State: store.PulpitMissing, Reason: reasonDeleted, PostedAt: now.Add(-6 * time.Hour)},
	}
	kept := withinHorizon(rows, now)
	outcomes := make([]outcome, 0, len(kept))
	for _, row := range kept {
		outcomes = append(outcomes, outcomeOf(row))
	}
	off, reason := fuseVerdict(fuseInput{Recent: outcomes, Profile: profileOK}, 3)
	if !off {
		t.Fatal("две свежие чистки обязаны гасить фичу")
	}
	if !strings.Contains(reason, "вычистили") {
		t.Errorf("причина: %q", reason)
	}
}
