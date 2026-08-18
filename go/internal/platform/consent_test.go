package platform

import (
	"strings"
	"testing"
)

// Документов ровно два, и порядок фиксирован: сперва общее согласие, потом
// распространение — согласиться на публикацию, не согласившись на обработку,
// бессмысленно.
func TestCurrentConsentDocs(t *testing.T) {
	docs, err := CurrentConsentDocs(Operator{})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Fatalf("документов %d, ожидалось 2", len(docs))
	}
	if docs[0].Kind != ConsentProcessing || docs[1].Kind != ConsentDistribution {
		t.Fatalf("порядок документов: %s, %s", docs[0].Kind, docs[1].Kind)
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
	// Индексация закрыта — X-Robots-Tag и robots.txt в internal/web.
	if !strings.Contains(byKind[ConsentDistribution], "noindex") {
		t.Error("в тексте не сказано про запрет индексации")
	}
	// IP не хранятся: колонки ip_hmac есть, но не заполняются.
	if !strings.Contains(byKind[ConsentProcessing], "IP-адреса") {
		t.Error("в тексте обработки не сказано про IP")
	}
}
