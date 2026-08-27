package narod

import (
	"strings"
	"testing"
)

// Одно зерно — один и тот же испорченный текст. Нужно не тестам, а отладке:
// без этого нельзя ни повторить жалобу, ни сравнить два прогона реплея.
func TestInjectErrorsIsDeterministic(t *testing.T) {
	pats := []ErrorPattern{
		{ID: "no_comma_before_chto", Rate: 40},
		{ID: VariantErrorID, Rate: 40, Norm: "общем", Variant: "обшем"},
	}
	text := "Я думаю, что в общем это правильно, и вообще сегодня хороший день"

	first := InjectErrors(text, pats, 7)
	for i := 0; i < 5; i++ {
		if got := InjectErrors(text, pats, 7); got != first {
			t.Fatalf("тот же текст с тем же зерном испорчен иначе:\n%q\n%q", first, got)
		}
	}
	if first == text {
		t.Fatal("при такой частоте ошибка обязана появиться")
	}
	// Другое зерно — другой выбор места (иначе «детерминированно» означало бы
	// «всегда в одном и том же месте»).
	var differs bool
	for seed := uint64(1); seed < 20 && !differs; seed++ {
		differs = InjectErrors(text, pats, seed) != first
	}
	if !differs {
		t.Error("зерно ни на что не влияет")
	}
}

func TestInjectErrorsRespectsRate(t *testing.T) {
	text := strings.Repeat("я думаю, что всё хорошо и понятно ", 30) // ~210 слов
	pats := []ErrorPattern{{ID: "no_comma_before_chto", Rate: 10}}   // ≈2 на текст

	var total int
	const runs = 200
	for seed := uint64(0); seed < runs; seed++ {
		got := InjectErrors(text, pats, seed)
		total += strings.Count(text, ", что") - strings.Count(got, ", что")
	}
	avg := float64(total) / runs
	if avg < 1.4 || avg > 2.6 {
		t.Errorf("в среднем внесено %.2f ошибок, ожидалось около 2", avg)
	}
}

// Короткая реплика чаще всего остаётся чистой — как у человека: частота
// замерена на тысячу слов, а реплики по шестьдесят знаков.
func TestInjectErrorsMostlySkipsShortText(t *testing.T) {
	text := "да ладно, что там"
	pats := []ErrorPattern{{ID: "no_comma_before_chto", Rate: 3}}
	var touched int
	const runs = 200
	for seed := uint64(0); seed < runs; seed++ {
		if InjectErrors(text, pats, seed) != text {
			touched++
		}
	}
	if share := float64(touched) / runs; share > 0.2 {
		t.Errorf("короткая реплика испорчена в %.0f%% прогонов — ошибка стала приметой, а не привычкой", share*100)
	}
}

func TestInjectorsByClass(t *testing.T) {
	cases := []struct {
		id      string
		text    string
		want    string
		wantAny []string
	}{
		{id: "no_comma_before_chto", text: "я думаю, что успею", want: "я думаю что успею"},
		{id: "no_space_after_comma", text: "да, конечно", want: "да,конечно"},
		{id: "lower_after_dot", text: "Пришёл. Ушёл", want: "Пришёл. ушёл"},
		{id: "long_ellipsis", text: "ну не знаю...", want: "ну не знаю...."},
		{id: "colloquial", text: "сегодня вообще тепло", wantAny: []string{"седня вообще тепло", "сегодня ваще тепло"}},
		{id: "tsya", text: "надо учиться дальше", want: "надо учится дальше"},
		{id: "tsya", text: "он учится в универе", want: "он учиться в универе"},
	}
	for _, tc := range cases {
		t.Run(tc.id+"/"+tc.text, func(t *testing.T) {
			// Частота подобрана так, чтобы вышла РОВНО одна вставка: при
			// нескольких проверялась бы уже не правка, а их наложение.
			pats := []ErrorPattern{{ID: tc.id, Rate: 1000 / float64(len(strings.Fields(tc.text)))}}
			got := InjectErrors(tc.text, pats, 3)
			if len(tc.wantAny) > 0 {
				for _, w := range tc.wantAny {
					if got == w {
						return
					}
				}
				t.Fatalf("вышло %q, ожидалось одно из %v", got, tc.wantAny)
			}
			if got != tc.want {
				t.Fatalf("вышло %q, ожидалось %q", got, tc.want)
			}
		})
	}
}

// Подмена в начале предложения не должна ронять заглавную: это была бы вторая,
// незамеренная ошибка.
func TestInjectVariantKeepsCase(t *testing.T) {
	pats := []ErrorPattern{{ID: VariantErrorID, Rate: 1000, Norm: "общем", Variant: "обшем"}}
	got := InjectErrors("Общем так и вышло", pats, 1)
	if got != "Обшем так и вышло" {
		t.Errorf("вышло %q", got)
	}
}

// Слово ищется целиком: «общем» внутри «общемировой» — не то слово.
func TestInjectVariantWholeWordOnly(t *testing.T) {
	pats := []ErrorPattern{{ID: VariantErrorID, Rate: 1000, Norm: "общем", Variant: "обшем"}}
	text := "это общемировой уровень"
	if got := InjectErrors(text, pats, 1); got != text {
		t.Errorf("подменена часть слова: %q", got)
	}
}

// Карточка может пережить код: незнакомый класс ошибки молча пропускается, а не
// роняет реплику.
func TestInjectErrorsIgnoresUnknownClass(t *testing.T) {
	text := "обычная реплика"
	if got := InjectErrors(text, []ErrorPattern{{ID: "чего-то новое", Rate: 1000}}, 1); got != text {
		t.Errorf("незнакомый класс что-то сделал с текстом: %q", got)
	}
}

func TestInjectErrorsEmptyInputs(t *testing.T) {
	if got := InjectErrors("", []ErrorPattern{{ID: "double_space", Rate: 1000}}, 1); got != "" {
		t.Errorf("пустой текст испорчен: %q", got)
	}
	if got := InjectErrors("текст", nil, 1); got != "текст" {
		t.Errorf("без образцов текст изменился: %q", got)
	}
}
