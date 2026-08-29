package narod

import (
	"strings"
	"testing"
)

func moodCard(gender, frust string) Card {
	return Card{ID: "x", Kind: KindComposite,
		Persona: Bio{Nick: "Кедрачъ", Gender: gender, Frustration: frust}}
}

// Больное место переводится в ПОВЕДЕНИЕ, а не в жалобу: названное вслух, оно
// даёт реплику «мне одиноко», которой человек не пишет.
func TestMoodTellsFrustrationAsBehaviour(t *testing.T) {
	got := WriteMood(MoodPoint{Card: moodCard("male", FrustLonely)})
	if !strings.Contains(got, "Больное место") {
		t.Fatalf("больное место не названо вовсе:\n%s", got)
	}
	if !strings.Contains(got, "не называешь") {
		t.Fatalf("не сказано, что вслух его не называют:\n%s", got)
	}
	if strings.Contains(got, FrustLonely) {
		t.Fatalf("вид подан словом из списка — модель перескажет его буквально:\n%s", got)
	}
}

// Запрет мата стоит В КАЖДОМ блоке, а не в одном режиме: platmod гасит брань
// сам, и реплика с ней — оплаченная генерация, снятая со страницы.
func TestMoodAlwaysBansProfanity(t *testing.T) {
	cases := []MoodPoint{
		{Card: moodCard("male", FrustLonely)},
		{Card: moodCard("female", FrustBody), Peer: "Ирма", PeerGender: "female", Heat: 3},
		{Card: moodCard("", ""), Peer: "Ирма", PeerGender: "male", Tone: -1, Heat: 9},
	}
	for i, p := range cases {
		if got := WriteMood(p); !strings.Contains(got, "Мата не пишешь") {
			t.Fatalf("случай %d без запрета мата:\n%s", i, got)
		}
	}
}

// Житель без больного места, пишущий в саму заметку, добавляет к промпту ноль:
// пустой заголовок «с каким чувством» — это предложение выдумать чувство.
func TestMoodSilentWhenNothingToSay(t *testing.T) {
	if got := WriteMood(MoodPoint{Card: moodCard("male", "")}); got != "" {
		t.Fatalf("пустое настроение всё же напечаталось:\n%q", got)
	}
}

// Лестница движется НАКАЛОМ и упирается в последнюю ступень: разговор,
// длящийся сотню реплик, не обязан заканчиваться ничем новым.
func TestMoodClimbsWithHeat(t *testing.T) {
	seen := map[string]bool{}
	for _, heat := range []int{0, 1, 2, 3, 4, 40} {
		got := WriteMood(MoodPoint{Card: moodCard("male", ""), Peer: "Ирма",
			PeerGender: "female", Tone: -0.5, Heat: heat})
		if !strings.Contains(got, "Сейчас:") {
			t.Fatalf("накал %d без ступени:\n%s", heat, got)
		}
		seen[got] = true
	}
	if len(seen) < 3 {
		t.Fatalf("лестница не движется: разных ступеней всего %d", len(seen))
	}
	top := WriteMood(MoodPoint{Card: moodCard("male", ""), Peer: "Ирма",
		PeerGender: "female", Tone: -0.5, Heat: 4})
	far := WriteMood(MoodPoint{Card: moodCard("male", ""), Peer: "Ирма",
		PeerGender: "female", Tone: -0.5, Heat: 400})
	if top != far {
		t.Fatalf("выше последней ступени нашлась ещё одна:\n%s\n---\n%s", top, far)
	}
}

// Тёплая пара идёт по СВОЕЙ лестнице: накал сам по себе знака не имеет, и
// подряд идущий обмен бывает не только ссорой.
func TestMoodWarmPairHasItsOwnLadder(t *testing.T) {
	cold := WriteMood(MoodPoint{Card: moodCard("male", ""), Peer: "Ирма",
		PeerGender: "female", Tone: -0.8, Heat: 2})
	warm := WriteMood(MoodPoint{Card: moodCard("male", ""), Peer: "Ирма",
		PeerGender: "female", Tone: 0.8, Heat: 2})
	if cold == warm {
		t.Fatal("вражда и приязнь дали одну и ту же ступень")
	}
	if strings.Contains(warm, "требуешь замолчать") {
		t.Fatalf("тёплая пара идёт по лестнице ссоры:\n%s", warm)
	}
}

// Подтекст живёт только у РАЗНОПОЛОЙ пары — там же, где замерен рычаг пола.
// Однополая пара и неизвестный пол строки не получают вовсе.
func TestMoodSubtextOnlyForMixedPair(t *testing.T) {
	mixed := WriteMood(MoodPoint{Card: moodCard("male", ""), Peer: "Ирма",
		PeerGender: "female", Heat: 1})
	if !strings.Contains(mixed, "разнополый") {
		t.Fatalf("разнополая пара без подтекста:\n%s", mixed)
	}
	same := WriteMood(MoodPoint{Card: moodCard("male", ""), Peer: "Гоша",
		PeerGender: "male", Heat: 1})
	if strings.Contains(same, "разнополый") {
		t.Fatalf("однополая пара получила подтекст:\n%s", same)
	}
	unknown := WriteMood(MoodPoint{Card: moodCard("male", ""), Peer: "Гоша", Heat: 1})
	if strings.Contains(unknown, "разнополый") {
		t.Fatalf("неизвестный пол дал подтекст:\n%s", unknown)
	}
}

// Реплика в саму заметку идёт без пары: ни ступени, ни подтекста у неё быть не
// может, а больное место человек приносит с собой.
func TestMoodWithoutPeerKeepsOnlyWhatIsHis(t *testing.T) {
	got := WriteMood(MoodPoint{Card: moodCard("male", FrustMoney), Tone: -1, Heat: 5})
	if strings.Contains(got, "Сейчас:") || strings.Contains(got, "разнополый") {
		t.Fatalf("у реплики без адресата завелась пара:\n%s", got)
	}
	if !strings.Contains(got, "Больное место") {
		t.Fatalf("больное место потерялось вместе с парой:\n%s", got)
	}
}

// Все виды закрытого списка обязаны быть переведены в поведение: вид, забытый в
// frustrationLine, молча выключал бы половину жителей.
func TestEveryFrustrationKindHasWords(t *testing.T) {
	for _, k := range FrustrationKinds {
		if frustrationLine(k) == "" {
			t.Fatalf("вид %q не переведён в поведение", k)
		}
	}
	if frustrationLine("тоска по несбывшемуся") != "" {
		t.Fatal("вид не из списка всё же напечатался")
	}
}
