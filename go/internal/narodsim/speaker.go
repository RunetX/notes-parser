package narodsim

// Speaker поверх готового цикла контроля голоса (archive.GenerateVoice).
//
// Своего цикла реплей не заводит намеренно. Тот, что есть, оплачен эпиком
// `voice`: он строит held-out-полосу по настоящим текстам автора, прогоняет
// черновик через атрибутор того же архива и возвращает КВАНТИЛЬ — долю
// настоящих текстов, узнанных хуже нашего. Это единственная мерка голоса,
// которая не сводится к «на глаз похоже», и заводить рядом вторую значило бы
// получить два числа, расходящихся молча.

import (
	"context"
	"fmt"

	"lovegw/internal/archive"
)

// branchLimit — сколько строк разговора показывать модели. Двадцать: столько же
// берёт `personas voice`, и расходиться с ним здесь нельзя — иначе калибровка
// мерила бы голос при одном контексте, а боевая генерация шла бы при другом.
const branchLimit = 20

// VoiceSpeaker — «что житель написал бы здесь», через цикл контроля.
type VoiceSpeaker struct {
	Store *archive.Store
	Gen   archive.JSONGenerator
	Card  *archive.VoiceCard
	// SelfIDs — анкеты самого жителя: по ним его прошлые слова в ветке
	// помечаются своими, иначе он противоречил бы сам себе.
	SelfIDs []int64
	// Req — шаблон запроса: черновики, раунды, пороги. Mode и Thread
	// подставляются на каждой точке.
	Req archive.VoiceRequest
}

// Speak генерирует реплику на месте настоящей.
func (s *VoiceSpeaker) Speak(ctx context.Context, p SpeechPoint) (Speech, error) {
	if s.Store == nil || s.Gen == nil || s.Card == nil {
		return Speech{}, fmt.Errorf("speaker: не задан архив, модель или карточка")
	}
	req := s.Req
	req.Mode = archive.VoiceModeComment
	req.Thread = archive.ScriptVoiceThread(p.Script, p.Upto, p.Truth.ReplyTo, s.SelfIDs, branchLimit)

	run, err := s.Store.GenerateVoice(ctx, s.Gen, s.Card, req, p.Now)
	if err != nil {
		return Speech{}, err
	}
	return speechOf(run), nil
}

// speechOf переводит итог цикла в строку отчёта.
//
// Пустой Best — это не ошибка прогона: все черновики могли выбыть на проверках,
// и «модель ничего не дала» само по себе результат, который обязан попасть в
// отчёт с причиной. Потерять его значило бы посчитать медиану по одним удачам.
func speechOf(run *archive.VoiceRun) Speech {
	if run == nil {
		return Speech{Rejected: "прогон не состоялся"}
	}
	if run.Best == nil {
		why := run.Verdict
		if why == "" {
			why = "ни один черновик не прошёл проверки"
		}
		if run.Aborted != "" {
			why = run.Aborted
		}
		return Speech{Rejected: why}
	}
	return Speech{
		Got:      run.Best.Text,
		Quantile: run.Best.Quantile,
		Rank:     run.Best.Score.Rank,
		Of:       run.Best.Score.Of,
		Copy:     run.Best.Copy,
	}
}
