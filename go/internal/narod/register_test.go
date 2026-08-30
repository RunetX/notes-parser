package narod

import (
	"strings"
	"testing"
)

func regCard() Register {
	return Register{
		ParenRuns:    map[string]float64{"1": 0.09, "2": 0, "3": 0.27, "4+": 0},
		NoFinalPunct: 0.75,
		StartsLower:  0.05,
		SmileyRate:   0.2,
		Smileys:      []Count{{Text: ":::popcorn:::", Share: 0.4}},
	}
}

// Скобочная подпись доводится до ЗАМЕРЕННОЙ, а не остаётся той, что поставила
// модель: три четверти наших реплик кончались ровно одной скобкой при 16 % у
// живых, и это был самый заметный признак машины.
func TestRegisterResizesParenRun(t *testing.T) {
	r := regCard()
	got := map[int]int{}
	for seed := uint64(0); seed < 400; seed++ {
		s := ApplyRegister("вот и весь ответ)", r, 0, seed)
		got[len(s)-len(strings.TrimRight(s, ")"))]++
	}
	// Замер говорит: одна скобка в 9 % реплик, три — в 27 %, двух и четырёх нет
	// вовсе, остальное без скобок. Проверяем ФОРМУ, а не точные доли.
	if got[2] != 0 || got[4] != 0 {
		t.Errorf("появились связки, которых у донора нет: %v", got)
	}
	if got[3] < got[1] {
		t.Errorf("тройная скобка реже одинарной, хотя замерена втрое чаще: %v", got)
	}
	if got[0] == 0 {
		t.Error("скобки не снимаются никогда, хотя у донора их нет в двух третях реплик")
	}
}

// Точка в конце снимается по замеру: у Севы её нет в 75 % реплик, а он писал с
// точками все до одной.
func TestRegisterDropsFinalDot(t *testing.T) {
	r := regCard()
	dropped, dots, ell := 0, 0, 0
	for seed := uint64(0); seed < 400; seed++ {
		s := ApplyRegister("гараж не иголка.", r, 0.2, seed)
		switch {
		case strings.HasSuffix(s, ".."):
			ell++
		case strings.HasSuffix(s, "."):
			dots++
		default:
			dropped++
		}
	}
	if dropped < dots {
		t.Errorf("точка остаётся чаще, чем снимается: снято %d, осталось %d", dropped, dots)
	}
	if ell == 0 {
		t.Error("многоточия не появилось ни разу")
	}
	// Вопрос и восклицание не трогаются вовсе: они несут смысл, а не привычку.
	if s := ApplyRegister("а гараж куда пропал?", r, 0.5, 7); !strings.HasSuffix(s, "?") {
		t.Errorf("сняли вопросительный знак: %q", s)
	}
}

// Пустая карточка ничего не делает: житель без замера остаётся как написан.
func TestRegisterSilentWithoutMeasurements(t *testing.T) {
	if got := ApplyRegister("текст.", Register{}, 0, 1); got != "текст." {
		t.Errorf("пустой замер изменил реплику: %q", got)
	}
}
