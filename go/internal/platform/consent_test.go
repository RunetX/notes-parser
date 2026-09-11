package platform

import (
	"strings"
	"testing"
)

// Документов ЧЕТЫРЕ, и порядок фиксирован: сперва общее согласие, потом
// распространение — согласиться на публикацию, не согласившись на обработку,
// бессмысленно, — а необязательные идут последними, потому что и читаются они
// последними (привязка мессенджера, затем личная переписка). Обязательных при
// этом по-прежнему ДВА, и это стережёт TestOptionalConsentIsNotAskedAtTheDoor:
// список документов площадки и список того, что спрашивают на входе, с
// 11.09.2026 разные вещи, и разводились они ровно затем, чтобы необязательный
// документ не мог однажды встать стеной на входе.
//
// Число берётся из allConsentKinds, а не пишется цифрой: двух мест, где сказано,
// сколько у площадки документов, быть не должно — расходятся они молча. А вот
// ПОРЯДОК назван поимённо: он и есть то, что стережёт этот тест.
func TestCurrentConsentDocs(t *testing.T) {
	docs, err := CurrentConsentDocs(Operator{})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != len(allConsentKinds) {
		t.Fatalf("документов %d, ожидалось %d", len(docs), len(allConsentKinds))
	}
	want := []string{ConsentProcessing, ConsentDistribution, ConsentBinding, ConsentTalks}
	for i, kind := range want {
		if docs[i].Kind != kind {
			t.Fatalf("документ %d: %s, ожидался %s", i, docs[i].Kind, kind)
		}
	}
	for _, d := range docs {
		if d.Version < 1 || d.Title == "" || len(d.SHA) != 32 {
			t.Errorf("%s: версия %d, заголовок %q", d.Kind, d.Version, d.Title)
		}
		if strings.Contains(d.Body, "{{") {
			t.Errorf("%s: в опубликованном тексте остался шаблон", d.Kind)
		}
	}
}

// Реквизиты оператора подставляются ДО публикации: доказательством служит
// финальный текст, а не шаблон, поэтому их смена обязана менять и хеш.
func TestOperatorGoesIntoTheText(t *testing.T) {
	plain, err := CurrentConsentDocs(Operator{})
	if err != nil {
		t.Fatal(err)
	}
	named, err := CurrentConsentDocs(Operator{Name: "ИП Иванов", Contact: "a@b.ru"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(named[0].Body, "ИП Иванов") {
		t.Error("реквизиты оператора не попали в текст")
	}
	if string(plain[0].SHA) == string(named[0].SHA) {
		t.Error("смена реквизитов не изменила хеш документа — значит, доказывать нечем")
	}
	// Пустые реквизиты дают безличную, но правдивую подпись, а не пустоту.
	if strings.Contains(plain[0].Body, "  —") || !strings.Contains(plain[0].Body, "Владелец площадки") {
		t.Error("без реквизитов текст должен называть оператора безлично")
	}
}

func TestParseConsentName(t *testing.T) {
	if _, _, err := parseConsentName("processing.txt"); err == nil {
		t.Error("имя без версии принято")
	}
	if _, _, err := parseConsentName("marketing.v1.txt"); err == nil {
		t.Error("неизвестный вид согласия принят")
	}
	kind, v, err := parseConsentName("distribution.v3.txt")
	if err != nil || kind != ConsentDistribution || v != 3 {
		t.Errorf("distribution.v3.txt → (%q, %d, %v)", kind, v, err)
	}
}

// Оба документа обязаны говорить то же, что делает код: тексты и поведение
// расходятся молча, и заметить это можно только так.
func TestConsentTextsSayWhatTheCodeDoes(t *testing.T) {
	docs, err := CurrentConsentDocs(Operator{})
	if err != nil {
		t.Fatal(err)
	}
	byKind := map[string]string{}
	for _, d := range docs {
		byKind[d.Kind] = d.Body
	}
	// Отзыв распространения исполняется немедленно — setOwnVisibility.
	if !strings.Contains(byKind[ConsentDistribution], "немедленно") {
		t.Error("в тексте распространения не сказано о немедленном отзыве")
	}
	// Индексация ОТКРЫТА (23.08.2026) — web.privatePath и handleRobots. Прежняя
	// редакция обещала обратное, потому и заменена: текст, разошедшийся с
	// поведением, обесценивает подпись под ним.
	if !strings.Contains(byKind[ConsentDistribution], "поисковым системам") {
		t.Error("в тексте распространения не сказано про поисковые системы")
	}
	if strings.Contains(byKind[ConsentDistribution], "noindex") {
		t.Error("в действующей редакции осталось обещание закрытой индексации")
	}
	// IP не хранятся: колонки ip_hmac есть, но не заполняются.
	if !strings.Contains(byKind[ConsentProcessing], "IP-адреса") {
		t.Error("в тексте обработки не сказано про IP")
	}
}
