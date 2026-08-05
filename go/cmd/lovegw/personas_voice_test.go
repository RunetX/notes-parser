package main

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenForVoice — пакеты, через которые текст мог бы уйти наружу: на сайт,
// в мессенджеры или в боевую БД с сессиями пользователей.
var forbiddenForVoice = []string{
	"lovegw/internal/store",
	"lovegw/internal/love",
	"lovegw/internal/tgx",
	"lovegw/internal/maxx",
	"lovegw/internal/dmbot",
	"lovegw/internal/mirror",
	"lovegw/internal/bridge",
	"lovegw/internal/news",
}

// TestVoiceHasNoPublishingPath — главный guardrail инструмента. personas voice
// имитирует письмо реального частного человека; у него не должно быть НИ ОДНОГО
// пути публикации. Проверяется списком импортов, а не обещанием в комментарии:
// комментарий не ломается, когда кто-то добавит отправку, а этот тест ломается.
func TestVoiceHasNoPublishingPath(t *testing.T) {
	imports := fileImports(t, "personas_voice.go")
	for _, bad := range forbiddenForVoice {
		if imports[bad] {
			t.Errorf("personas_voice.go импортирует %s — появился путь наружу; "+
				"инструмент обязан оставаться read-only по архиву и писать только в dump/", bad)
		}
	}
}

// TestVoiceEngineHasNoLLMImport — движок карты и скоринга не должен знать про
// конкретного LLM-провайдера: онлайн-модель подаётся интерфейсом с уровня CLI
// (тот же шов, что JSONGenerator в digest).
func TestVoiceEngineHasNoLLMImport(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "internal", "archive", "voice*.go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("не найдено ни одного internal/archive/voice*.go")
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		for imp := range fileImportsAt(t, f) {
			if strings.HasPrefix(imp, "lovegw/internal/llm") {
				t.Errorf("%s импортирует %s — движок должен получать модель интерфейсом", f, imp)
			}
		}
	}
}

func fileImports(t *testing.T, name string) map[string]bool {
	t.Helper()
	return fileImportsAt(t, name)
}

func fileImportsAt(t *testing.T, path string) map[string]bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("разбор %s: %v", path, err)
	}
	out := map[string]bool{}
	for _, imp := range f.Imports {
		out[strings.Trim(imp.Path.Value, `"`)] = true
	}
	return out
}
