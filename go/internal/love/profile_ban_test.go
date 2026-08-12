package love

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Разметка страниц настроек — синтетическая, а не записанная фикстура: живая
// /properties/ несёт почту, ник и id владельца, и в testdata ей не место
// (testdata не гитигнорится). Воспроизведено то, что существенно и снято с
// живого сайта: isAuth, isActive и форма кнопки.
//
// activeHTML — анкета активна: кнопка блокировки и СОСЕДНЯЯ форма удаления с
// тем же action.
func activeHTML(banField, banLabel string) string {
	return `<html><head><script>
  window.dataFromBlade = {"searchResult":[],"layout":{"user":{"id":1,"isAuth":true,` +
		`"isGuest":false,"isActive":true,"profileBlockState":null}}};
</script></head><body>
  <h6>Блокировка профиля</h6>
  <form action="/properties/ban/" method="post">
    <input type="submit" class="custom-btn js-self-ban" name="` + banField + `" value="` + banLabel + `" />
  </form>
  <h6>Удаление профиля</h6>
  <form action="/properties/ban/" method="post">
    <input type="submit" class="custom-btn js-self-delete" name="delete" value="Удалить профиль" />
  </form>
</body></html>`
}

// blockedHTML — анкета заблокирована. Разметка ровно как на сайте: класса
// js-self-ban нет, формы удаления нет, а в name кнопки склеен список классов.
const blockedHTML = `<html><head><script>
  window.dataFromBlade = {"layout":{"user":{"isAuth":true,"isActive":false,"profileBlockState":null}}};
</script></head><body>
  <h6>Разблокировка профиля</h6>
  <form action="/properties/ban/" method="post">
    <input type="submit" class="custom-btn" name="un_ban lv-user-properties__submit-btn" value="Разблокировать профиль" />
  </form>
</body></html>`

const guestPropertiesHTML = `<html><head><script>
  window.dataFromBlade = {"layout":{"user":{"isAuth":false,"isGuest":true}}};
</script></head><body>Войдите</body></html>`

// propertiesServer — сайт из одной страницы настроек; возвращает клиент и
// указатель на тело последнего POST (пустое — запрос не уходил).
func propertiesServer(t *testing.T, page string) (*Client, *string) {
	t.Helper()
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			body = string(b)
			w.Write([]byte("ok"))
			return
		}
		w.Write([]byte(page))
	}))
	t.Cleanup(srv.Close)
	return testClient(t, srv), &body
}

func TestProfileControlReadsActiveProfile(t *testing.T) {
	c, _ := propertiesServer(t, activeHTML("ban", "Заблокировать профиль"))
	ctrl, err := c.ProfileControl(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if ctrl.Blocked {
		t.Error("profileBlockState=null — анкета не заблокирована")
	}
	if !ctrl.Available {
		t.Fatal("кнопка блокировки не найдена")
	}
	if ctrl.Label != "Заблокировать профиль" {
		t.Errorf("подпись кнопки: %q", ctrl.Label)
	}
	if ctrl.field != "ban" || ctrl.action != "/properties/ban/" {
		t.Errorf("форма: поле %q, action %q", ctrl.field, ctrl.action)
	}
}

// Заблокированная анкета: состояние из isActive, а поле — ПЕРВОЕ слово name.
// Строку целиком сервер игнорирует, это проверено на живой анкете.
func TestProfileControlReadsBlockedProfile(t *testing.T) {
	c, _ := propertiesServer(t, blockedHTML)
	ctrl, err := c.ProfileControl(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ctrl.Blocked {
		t.Error("isActive:false — анкета заблокирована")
	}
	if ctrl.field != "un_ban" || ctrl.Label != "Разблокировать профиль" {
		t.Errorf("кнопка снята неверно: поле %q, подпись %q", ctrl.field, ctrl.Label)
	}
}

func TestProfileControlBlockedWithoutButton(t *testing.T) {
	// Кнопки может не быть вовсе — это не дрейф вёрстки: состояние известно,
	// нажимать нечего.
	page := `<html><head><script>window.dataFromBlade = {"layout":{"user":` +
		`{"isAuth":true,"isActive":false}}};</script></head><body></body></html>`
	c, _ := propertiesServer(t, page)
	ctrl, err := c.ProfileControl(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ctrl.Blocked || ctrl.Available {
		t.Errorf("ожидали «заблокирована, кнопки нет», получили %+v", ctrl)
	}
}

func TestProfileControlGuestIsUnauthorized(t *testing.T) {
	c, _ := propertiesServer(t, guestPropertiesHTML)
	_, err := c.ProfileControl(context.Background(), nil)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("гостю ждём ErrUnauthorized, получили %v", err)
	}
}

func TestProfileControlMarkupDrift(t *testing.T) {
	c, _ := propertiesServer(t, "<html><body>совсем другая страница</body></html>")
	_, err := c.ProfileControl(context.Background(), nil)
	var me *MarkupError
	if !errors.As(err, &me) {
		t.Fatalf("ждём MarkupError, получили %v", err)
	}
}

func TestSubmitProfileControlSendsExactlyTheSiteForm(t *testing.T) {
	c, body := propertiesServer(t, activeHTML("ban", "Заблокировать профиль"))
	ctx := context.Background()
	ctrl, err := c.ProfileControl(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SubmitProfileControl(ctx, nil, ctrl); err != nil {
		t.Fatal(err)
	}
	if got := *body; got != "ban=%D0%97%D0%B0%D0%B1%D0%BB%D0%BE%D0%BA%D0%B8%D1%80%D0%BE%D0%B2%D0%B0%D1%82%D1%8C+%D0%BF%D1%80%D0%BE%D1%84%D0%B8%D0%BB%D1%8C" {
		t.Errorf("тело POST: %q", got)
	}
	if strings.Contains(*body, "delete") {
		t.Error("в запросе не должно быть поля удаления анкеты")
	}
}

// Возврат анкеты уходит чистым полем: строку с классами сервер игнорирует,
// анкета остаётся закрытой (проверено живьём).
func TestSubmitProfileControlSendsCleanUnbanField(t *testing.T) {
	c, body := propertiesServer(t, blockedHTML)
	ctx := context.Background()
	ctrl, err := c.ProfileControl(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SubmitProfileControl(ctx, nil, ctrl); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(*body, "un_ban=") {
		t.Errorf("тело POST должно начинаться с чистого un_ban=, а не с классов: %q", *body)
	}
	if strings.Contains(*body, "properties__submit-btn") {
		t.Errorf("в имени поля не должно быть классов: %q", *body)
	}
}

// Соседняя форма — удаление анкеты, и цена ошибки необратима: подхватить её
// как кнопку нельзя ни при каком дрейфе вёрстки. Отбор — белый список полей,
// поэтому страница, где остались одни delete-кнопки, даёт «нажимать нечего».
func TestProfileControlNeverPicksDeleteField(t *testing.T) {
	c, _ := propertiesServer(t, activeHTML("delete", "Удалить профиль"))
	ctrl, err := c.ProfileControl(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if ctrl.Available {
		t.Fatalf("удаление анкеты не должно стать кнопкой: поле %q", ctrl.field)
	}
}

func TestSubmitProfileControlRefusesDeleteField(t *testing.T) {
	c, body := propertiesServer(t, activeHTML("ban", "Заблокировать профиль"))
	ctrl := ProfileControl{Available: true, action: "/properties/ban/", field: "delete", value: "Удалить профиль"}
	if err := c.SubmitProfileControl(context.Background(), nil, ctrl); err == nil {
		t.Fatal("удаление анкеты должно быть отвергнуто")
	}
	if *body != "" {
		t.Errorf("запрос не должен был уйти, ушло: %q", *body)
	}
}

func TestSubmitProfileControlWithoutButton(t *testing.T) {
	c, body := propertiesServer(t, activeHTML("ban", "Заблокировать профиль"))
	if err := c.SubmitProfileControl(context.Background(), nil, ProfileControl{Blocked: true}); err == nil {
		t.Fatal("нажимать нечего — ждём ошибку")
	}
	if *body != "" {
		t.Errorf("запрос не должен был уйти, ушло: %q", *body)
	}
}
