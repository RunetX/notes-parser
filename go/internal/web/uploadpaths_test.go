package web

// Пути с картинкой живут в ДВУХ местах: `uploadPaths` здесь и матчер @shot в
// deploy/platform/Caddyfile. Свести их в одно нельзя — Caddy не читает Go, — но
// расхождение стоило двух боевых дефектов, и оба выглядели не тем, чем были:
//
//   - 27.08.2026 правку заметки добавили в Go и забыли в Caddyfile: честная
//     картинка в 2,9 МБ ловила 413, обрезанная на первом мегабайте;
//   - 30.08.2026 завели фото жителю (/mod/admin/avatar) и не тронули НИ ОДНОГО
//     из двух списков: тело резалось потолком текстовой формы в 64 КиБ, а на
//     экране это выглядело ЗАВИСАНИЕМ — сервер перестаёт читать тело, пока
//     браузер его ещё шлёт, и отказа человек не видит вовсе.
//
// В комментарии Caddyfile до сегодня стояло «тестом это не ловится». Ловится:
// файл лежит в репозитории, а список в нём — обычные слова.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

const caddyfilePath = "../../../deploy/platform/Caddyfile"

// shotMatcher вытаскивает строку `path …` из матчера @shot.
var shotMatcher = regexp.MustCompile(`(?s)@shot\s*\{.*?\n\s*path\s+([^\n]+)\n`)

func TestПутиСКартинкойСовпадаютСCaddy(t *testing.T) {
	raw, err := os.ReadFile(caddyfilePath)
	if err != nil {
		// НЕ Skip: файл лежит в репозитории по стабильному пути, и его
		// исчезновение означает, что сверять стало нечем, — а молчаливое
		// «сверять нечем» это ровно то состояние, из-за которого списки и
		// разъезжались.
		t.Fatalf("не читается %s: %v", caddyfilePath, err)
	}
	m := shotMatcher.FindSubmatch(raw)
	if m == nil {
		t.Fatal("в Caddyfile не нашёлся матчер @shot со строкой path")
	}
	inCaddy := strings.Fields(string(m[1]))

	if len(inCaddy) != len(uploadPaths) {
		t.Fatalf("путей с картинкой в Caddy %d, в Go %d:\n  Caddy: %v\n  Go:    %v",
			len(inCaddy), len(uploadPaths), inCaddy, uploadPaths)
	}
	// Порядок сверяем тоже: списки правят вместе, и разошедшийся порядок это
	// признак, что правил их не один человек и не за один раз.
	for i, p := range uploadPaths {
		if inCaddy[i] != p {
			t.Errorf("путь %d: в Go %q, в Caddy %q", i, p, inCaddy[i])
		}
	}
}

// Тот же список стоит и в ОТРИЦАНИИ (@notshot): матчеры взаимоисключающие, и
// путь, попавший в один и забытый в другом, получил бы оба потолка сразу — а
// request_body оставляет последний, а не больший.
func TestОтрицаниеCaddyПовторяетТотЖеСписок(t *testing.T) {
	raw, err := os.ReadFile(caddyfilePath)
	if err != nil {
		t.Fatalf("не читается %s: %v", caddyfilePath, err)
	}
	want := strings.Join(uploadPaths, " ")
	if n := strings.Count(string(raw), "path "+want); n != 2 {
		t.Errorf("строка путей встречается в Caddyfile %d раз, ожидалось 2 (@shot и @notshot)", n)
	}
}

// А это уже про сам Go: фото жителя обязано считаться приёмом файла. Тест
// назван отдельно от списка потому, что дефект был не в потолке, а в том, что
// путь в список не попал вовсе.
func TestФотоЖителяЭтоПриёмФайла(t *testing.T) {
	if !isUpload(postReq("/mod/admin/avatar")) {
		t.Error("фото жителя не считается приёмом файла: тело обрежется потолком текстовой формы")
	}
	if got := maxBodyOf(postReq("/mod/admin/avatar")); got != uploadMaxBytes {
		t.Errorf("потолок тела %d, ожидался %d", got, uploadMaxBytes)
	}
	if got := budgetOf(postReq("/mod/admin/avatar")); got != uploadBudget {
		t.Errorf("срок запроса %v, ожидался %v", got, uploadBudget)
	}
	// Соседние адреса под /mod/admin файла не принимают: выдача приглашений и
	// роли — обычные текстовые формы.
	if isUpload(postReq("/mod/admin")) {
		t.Error("вся страница администрирования принята за приём файла")
	}
}

// Звёздочка в середине работает так же, как у Caddy: начало и конец.
func TestЗвёздочкаВПутиЛовитНомерЗаметки(t *testing.T) {
	for _, c := range []struct {
		path string
		want bool
	}{
		{"/n/313128/edit", true},
		{"/n/100000000026/edit", true},
		{"/n/313128/reply", false},
		{"/nedit", false},
	} {
		if got := isUpload(postReq(c.path)); got != c.want {
			t.Errorf("%s: приём файла = %v, ожидалось %v", c.path, got, c.want)
		}
	}
}
