package pulpit

import (
	"testing"
	"time"
)

// TestDecide — решение о необратимой публикации. Порядок проверок и есть
// приоритет, поэтому таблица идёт от «молчим вовсе» к «пишем».
func TestDecide(t *testing.T) {
	base := decideInput{Enabled: true, Freshness: 15 * time.Minute, MaxPerDay: 25}

	cases := []struct {
		name   string
		in     decideInput
		want   action
		reason string
	}{
		{
			name: "свежая заметка при включённой фиче",
			in:   base,
			want: actPreach,
		},
		{
			name:   "тумблер выключен — в БД ни строчки",
			in:     decideInput{Enabled: false, Cold: true, Age: time.Hour},
			want:   actIdle,
			reason: reasonDisabled,
		},
		{
			name:   "заметку занял другой вход",
			in:     withInput(base, func(in *decideInput) { in.Taken = true }),
			want:   actIdle,
			reason: reasonTaken,
		},
		{
			name:   "холодный старт сильнее свежести",
			in:     withInput(base, func(in *decideInput) { in.Cold = true }),
			want:   actMark,
			reason: reasonColdStart,
		},
		{
			name:   "заметка успела обжиться",
			in:     withInput(base, func(in *decideInput) { in.Age = 20 * time.Minute }),
			want:   actMark,
			reason: reasonStale,
		},
		{
			name:   "суточный предохранитель",
			in:     withInput(base, func(in *decideInput) { in.Today = 25 }),
			want:   actMark,
			reason: reasonQuota,
		},
		{
			name: "потолок не задан — не ограничивает",
			in: withInput(base, func(in *decideInput) {
				in.Today, in.MaxPerDay = 100, 0
			}),
			want: actPreach,
		},
		{
			name: "занятая заметка молчит даже на холодном старте",
			in: withInput(base, func(in *decideInput) {
				in.Taken, in.Cold = true, true
			}),
			want:   actIdle,
			reason: reasonTaken,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			act, reason := decide(tc.in)
			if act != tc.want {
				t.Errorf("действие %v, ожидалось %v", act, tc.want)
			}
			if tc.reason != "" && reason != tc.reason {
				t.Errorf("причина %q, ожидалась %q", reason, tc.reason)
			}
		})
	}
}

func withInput(in decideInput, edit func(*decideInput)) decideInput {
	edit(&in)
	return in
}
