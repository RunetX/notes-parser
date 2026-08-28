package narodsim

import (
	"testing"

	"lovegw/internal/archive"
	"lovegw/internal/narod"
)

// Удача переносится в отчёт целиком: квантиль это мерка голоса, ранг и
// пересечение с образцами — то, чем она объясняется.
func TestSpeechOfCarriesMetrics(t *testing.T) {
	run := &archive.VoiceRun{Best: &archive.VoiceDraft{
		Text: "нафига оно надо", Quantile: 0.62, Copy: 0.11,
		Score: archive.VoiceScore{Rank: 3, Of: 1200},
	}}
	got := speechOf(run)
	if got.Got != "нафига оно надо" || got.Quantile != 0.62 ||
		got.Rank != 3 || got.Of != 1200 || got.Copy != 0.11 {
		t.Errorf("потеряна часть замера: %+v", got)
	}
	if got.Rejected != "" {
		t.Errorf("удачный прогон помечен отказом: %q", got.Rejected)
	}
}

// «Модель ничего не дала» — это результат с причиной, а не ошибка прогона.
// Потеряв его, мы считали бы медиану по одним удачам, то есть завышали бы
// качество ровно там, где оно провалилось.
func TestSpeechOfKeepsFailureReason(t *testing.T) {
	cases := []struct {
		name string
		run  *archive.VoiceRun
		want string
	}{
		{"нет прогона", nil, "прогон не состоялся"},
		{"вердикт цикла", &archive.VoiceRun{Verdict: "все черновики набиты словарём"},
			"все черновики набиты словарём"},
		{"без вердикта", &archive.VoiceRun{}, "ни один черновик не прошёл проверки"},
		// Обрыв позднего раунда сильнее вердикта: он объясняет пустоту точнее.
		{"обрыв раунда", &archive.VoiceRun{Verdict: "не принято", Aborted: "раунд 2 не состоялся: таймаут"},
			"раунд 2 не состоялся: таймаут"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := speechOf(tc.run)
			if got.Rejected != tc.want {
				t.Errorf("причина %q, ожидалась %q", got.Rejected, tc.want)
			}
			if got.Got != "" {
				t.Errorf("у отказа есть текст: %q", got.Got)
			}
		})
	}
}

// Отказавшие точки не портят медиану: она считается по тому, что вышло.
func TestMedianQuantileIgnoresRejected(t *testing.T) {
	r := &SoloRun{Speech: []Speech{
		{Quantile: 0.4},
		{Rejected: "нечего показать"},
		{Quantile: 0.8},
		{Quantile: 0.6},
	}}
	if got := r.MedianQuantile(); got != 0.6 {
		t.Errorf("медиана %v, ожидалось 0.6", got)
	}
}

func TestVoiceSpeakerNeedsParts(t *testing.T) {
	var s VoiceSpeaker
	if _, err := s.Speak(t.Context(), SpeechPoint{}); err == nil {
		t.Fatal("пустой speaker принят")
	}
}

// Длина называется НА КАЖДУЮ реплику отдельно и берётся жребием по всему
// разбросу донора, а не по его середине.
//
// Правило оплачено замером 28.08.2026: промпт просил «цель p25–p75» — то есть
// среднюю половину, — и у всех трёх слепков медиана вышла на треть ВЫШЕ
// донорской при p90 на четверть НИЖЕ. Разброс живёт МЕЖДУ репликами: каждая
// реплика человека имеет какую-то одну длину, а «обычную» модель выбирала
// каждый раз заново.
func TestTargetRunesSpreadsAcrossReplies(t *testing.T) {
	s := &VoiceSpeaker{Runes: narod.Dist{P10: 30, Median: 75, P90: 174, Max: 970}, Seed: 1}
	var short, long int
	for id := int64(1); id <= 200; id++ {
		switch n := s.targetRunes(id); {
		case n <= 0:
			t.Fatalf("реплика %d получила длину %d", id, n)
		case n < 60:
			short++
		case n > 150:
			long++
		}
	}
	// И короткие, и длинные обязаны встречаться: ради этого всё и заведено.
	if short < 20 || long < 10 {
		t.Errorf("разброса нет: коротких %d, длинных %d из 200", short, long)
	}
}

// Жребий длины воспроизводим и НЕ совпадает с жребием кубика: без соли длинная
// реплика приходилась бы ровно на приход жителя.
func TestTargetRunesDeterministicAndDecorrelated(t *testing.T) {
	s := &VoiceSpeaker{Runes: narod.Dist{P10: 30, Median: 75, P90: 174, Max: 970}, Seed: 1}
	if a, b := s.targetRunes(42), s.targetRunes(42); a != b {
		t.Errorf("жребий не воспроизводится: %d и %d", a, b)
	}
	same := 0
	for id := int64(1); id <= 100; id++ {
		if s.targetRunes(id) == (&VoiceSpeaker{Runes: s.Runes, Seed: 1 ^ lengthSalt}).targetRunes(id) {
			same++
		}
	}
	if same > 90 {
		t.Errorf("соль не разводит потоки: совпадений %d из 100", same)
	}
}

// Пустой замер длины молчит, а не выдумывает число.
func TestTargetRunesEmpty(t *testing.T) {
	s := &VoiceSpeaker{}
	if got := s.targetRunes(1); got != 0 {
		t.Errorf("по пустому разбросу получено %d", got)
	}
}
