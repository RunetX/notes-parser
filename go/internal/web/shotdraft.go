package web

// Черновик картинки — перекодированные байты, которые ЖДУТ своей заметки.
//
// До 28.08.2026 файл ехал в теле той же формы, что и текст, и это давало три
// беды разом. Человек узнавал об отказе («это не картинка», «слишком много
// точек») только ПОСЛЕ того, как написал заметку, — а браузер выбранный файл
// обратно не отдаёт, и приложить его надо было заново. Отправка выглядела
// зависанием: десять мегабайт с телефона ползут минуту, и всё это время экран
// молчит. И третье, невидимое: те же десять мегабайт занимали слот морды (их
// всего двенадцать) вместе с транзакцией публикации.
//
// Теперь файл уходит СРАЗУ по выбору, отдельным путём (POST /shot), а форма
// несёт лишь его номер. Кнопка отправки на время закачки неактивна — это и есть
// весь видимый смысл затеи: пока картинка едет, публиковать нечего.
//
// ЛЕЖИТ ЧЕРНОВИК В ПАМЯТИ, А НЕ НА ДИСКЕ, и это решение владельца (28.08.2026),
// у которого есть цена в обе стороны. На диске он был бы проще — хранилище
// адресуемо содержимым, повтор ничего не удваивает, — но уборки каталога у
// площадки нет вовсе: файл, выбранный и не опубликованный, остался бы там
// навсегда, а дверь эта открыта любому участнику. В памяти же он смертен по
// устройству: переживает отказ публикации (ради чего всё и делалось), но не
// переживает рестарт морды — и тогда страница честно просит приложить картинку
// заново. Это единственный случай, когда человек теряет выбранный файл, и он
// редкий: между выбором и отправкой проходят минуты, а морда перезапускается
// раз в выкатку.
//
// Потолки здесь считают ПАМЯТЬ, а не запросы, — тем же счётом, что shotsInFlight
// в shot.go, и по той же причине: контейнер морды живёт в трёхстах мегабайтах.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strconv"
	"sync"
	"time"

	"lovegw/internal/platform"
)

const (
	// draftField — имя поля формы, в котором едет номер черновика.
	draftField = "shot_id"

	// draftTTL — сколько черновик ждёт свою заметку. Полчаса: столько пишется
	// длинная заметка с отвлечениями, а больше держать чужие байты незачем —
	// человек, ушедший на час, вернётся к пустой форме в любом случае.
	draftTTL = 30 * time.Minute

	// draftsMax — сколько черновиков помним разом. Тридцать два при потолке
	// «один на человека» — это тридцать два человека, одновременно пишущих
	// заметку с картинкой; живых потоков у площадки шестьдесят четыре ВСЕГО,
	// так что до этого числа доходить неоткуда.
	draftsMax = 32

	// draftsBytes — и потолок по весу, потому что штуки веса не считают.
	// Перекодированная картинка (1600 точек по длинной стороне, webp) весит
	// две-три сотни килобайт, JPEG — вдвое больше; двадцать четыре мегабайта
	// это тот же тридцать второй черновик, только посчитанный честно.
	draftsBytes = 24 << 20
)

// shotDraft — одна ждущая картинка.
type shotDraft struct {
	shot  Shot
	owner int64
	at    time.Time
}

// shotDrafts — склад черновиков, один на процесс.
//
// Уборка ленивая, своей горутины нет: чистка стоит одного прохода по карте из
// трёх десятков строк и делается тем, кто и так держит мьютекс, — тот же приём,
// что у корзин в guard.go.
type shotDrafts struct {
	mu  sync.Mutex
	m   map[string]*shotDraft
	sz  int              // сколько байт лежит сейчас
	now func() time.Time // подменяется тестом
}

func newShotDrafts() *shotDrafts {
	return &shotDrafts{m: make(map[string]*shotDraft), now: time.Now}
}

// put кладёт черновик и возвращает его номер.
//
// Прежний черновик того же человека ВЫТЕСНЯЕТСЯ: картинка у заметки одна, а
// перевыбор файла — обычное дело, и без вытеснения склад рос бы ровно от того,
// что человек передумал.
func (d *shotDrafts) put(owner int64, shot Shot) string {
	id := draftID()

	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweepLocked()
	for k, v := range d.m {
		if v.owner == owner {
			d.dropLocked(k)
		}
	}
	// Место освобождается СТАРЫМИ, а не отказом новому: отказ пришёлся бы на
	// того, кто прямо сейчас смотрит на форму, а самый старый черновик — это
	// чаще всего тот, кто уже ушёл. Совсем без вытеснения обойтись нельзя:
	// склад в памяти обязан иметь верхний край.
	for len(d.m) >= draftsMax || d.sz+len(shot.Data) > draftsBytes {
		if !d.dropOldestLocked() {
			break
		}
	}
	d.m[id] = &shotDraft{shot: shot, owner: owner, at: d.now()}
	d.sz += len(shot.Data)
	return id
}

// get отдаёт черновик его владельцу. Чужой не отдаётся и не признаётся
// существующим: номер случаен, но проверка по владельцу дешевле веры в это.
func (d *shotDrafts) get(owner int64, id string) (Shot, bool) {
	if id == "" {
		return Shot{}, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweepLocked()
	v, ok := d.m[id]
	if !ok || v.owner != owner {
		return Shot{}, false
	}
	return v.shot, true
}

// drop убирает использованный черновик. Зовётся ПОСЛЕ удачной публикации, а не
// при чтении: отказ ядра (бан, частота, истёкшее согласие) не должен стоить
// человеку картинки — ровно ради этого черновик и переживает отказ.
func (d *shotDrafts) drop(id string) {
	if id == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dropLocked(id)
}

// len — сколько черновиков лежит. Только для тестов и диагностики.
func (d *shotDrafts) len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.m)
}

func (d *shotDrafts) dropLocked(id string) {
	if v, ok := d.m[id]; ok {
		d.sz -= len(v.shot.Data)
		delete(d.m, id)
	}
}

func (d *shotDrafts) sweepLocked() {
	now := d.now()
	for k, v := range d.m {
		if now.Sub(v.at) > draftTTL {
			d.dropLocked(k)
		}
	}
}

func (d *shotDrafts) dropOldestLocked() bool {
	var oldest string
	var at time.Time
	for k, v := range d.m {
		if oldest == "" || v.at.Before(at) {
			oldest, at = k, v.at
		}
	}
	if oldest == "" {
		return false
	}
	d.dropLocked(oldest)
	return true
}

// draftID — номер черновика: шестнадцать случайных байт. Не счётчик и не хеш
// содержимого, потому что номер уезжает в разметку и в адрес превью, а по
// угадываемому номеру чужая ещё не опубликованная картинка стала бы видна.
func draftID() string {
	var b [16]byte
	// crypto/rand.Read с Go 1.24 ошибки не возвращает — при отказе системного
	// источника он паникует сам (тот же довод, что у соли в guard.go).
	rand.Read(b[:]) //nolint:errcheck // см. выше
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// ---------------------------------------------------------------- маршруты

// shotDraftView — карточка черновика на форме: показанное превью плюс скрытое
// поле с номером. Тот же тип рисует и ответ POST /shot, и перерисованную после
// отказа форму — второго способа показать выбранную картинку не заводится, по
// той же причине, по которой его нет у живого добора и формы ответа.
type shotDraftView struct {
	ID      string
	Width   int
	Height  int
	Problem string
}

// handleShotUpload — предзагрузка картинки, ДО публикации.
//
// Порядок проверок тот же, что у публикации, и по той же причине — это порядок
// их ЦЕНЫ: происхождение читается из заголовков даром, вошедший уже лежит в
// контексте, право — один дешёвый запрос, и только потом слот перекодирования,
// чтение десяти мегабайт и ffmpeg.
//
// Отвечает он ГОТОВОЙ разметкой, а не JSON'ом с адресом файла: собери карточку
// скрипт сам — у площадки появилась бы вторая поверхность для XSS и второе
// место, где однажды разойдутся правила показа. Тот же приём, что у
// replyform.go и fresh.go.
func (s *Server) handleShotUpload(w http.ResponseWriter, r *http.Request) {
	if s.shots == nil || s.drafts == nil {
		// Картинок площадка сейчас не принимает — значит и пути такого нет, как
		// нет /mod у постороннего.
		s.fail(w, r, http.StatusNotFound, "Такой страницы нет.")
		return
	}
	if !sameOrigin(r) {
		s.fail(w, r, http.StatusForbidden, "Запрос пришёл не с нашей страницы.")
		return
	}
	u, ok := s.writer(w, r)
	if !ok {
		return
	}
	if err := s.mayDraft(r.Context(), u); err != nil {
		status, problem := writeProblem(err)
		if problem == "" {
			s.oops(w, r, "приём картинки", err)
			return
		}
		s.fail(w, r, status, problem)
		return
	}
	release, ok := s.takeShotSlot(w, r)
	if !ok {
		return
	}
	defer release()
	if !s.postUpload(w, r) {
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	shot, problem := s.takeShot(r.Context(), r)
	switch {
	case problem != "":
		// Негодный файл — это не наша авария, а разговор с человеком, поэтому
		// ответ тот же кусок разметки, только с отказом внутри. 400 нужен
		// скрипту: по нему он знает, что показывать нечего и выбор надо снять.
		s.renderDraft(w, r, http.StatusBadRequest, shotDraftView{Problem: problem})
	case shot == nil:
		s.fail(w, r, http.StatusBadRequest, "Файл не выбран.")
	default:
		id := s.drafts.put(u.ID, *shot)
		s.renderDraft(w, r, http.StatusOK, shotDraftView{ID: id, Width: shot.Width, Height: shot.Height})
	}
}

// mayDraft — кому можно прислать файл заранее.
//
// «Может публиковать ИЛИ администратор», и вторая половина не послабление:
// MayPublishNote считает и ЧАСТОТУ заметок, а администратор, ставящий картинку
// зеркальной заметке, ничего не публикует — своих заметок у него может не быть
// вовсе, а пять за сутки он мог написать и до того.
//
// Проверка тут вообще стоит не ради диска (черновик лежит в памяти и уберётся
// сам), а ради процессора: перекодирование это ffmpeg, и тратить его на того,
// кому публиковать всё равно откажут, незачем.
func (s *Server) mayDraft(ctx context.Context, u platform.User) error {
	if (platform.Viewer{UserID: u.ID, Role: u.Role}).CanAdmin() {
		return nil
	}
	return s.wr.MayPublishNote(ctx, u.ID)
}

// handleShotFile отдаёт черновик его владельцу — это превью на форме.
//
// Мимо хранилища и мимо Caddy: файла на диске ещё нет и, если заметку не
// опубликуют, не будет вовсе. Отсюда же no-store: у картинки, которой ещё нет,
// кэшироваться нечему.
func (s *Server) handleShotFile(w http.ResponseWriter, r *http.Request) {
	if s.drafts == nil {
		s.fail(w, r, http.StatusNotFound, "Такой страницы нет.")
		return
	}
	me, ok := s.me(r)
	if !ok {
		s.fail(w, r, http.StatusNotFound, "Такой страницы нет.")
		return
	}
	shot, ok := s.drafts.get(me.ID, r.PathValue("id"))
	if !ok {
		// Чужой и протухший отвечают одинаково: существование чужого черновика
		// — само по себе сведения.
		s.fail(w, r, http.StatusNotFound, "Такой картинки нет.")
		return
	}
	h := w.Header()
	h.Set("Content-Type", shot.MIME)
	h.Set("Content-Length", strconv.Itoa(len(shot.Data)))
	h.Set("Cache-Control", "private, no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Vary", "Cookie")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(shot.Data)
}

// renderDraft отдаёт карточку черновика куском разметки.
func (s *Server) renderDraft(w http.ResponseWriter, r *http.Request, status int, v shotDraftView) {
	var buf bytes.Buffer
	if err := s.renderPart(&buf, "shotdraft", v); err != nil {
		s.oops(w, r, "показ картинки", err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "private, no-store")
	h.Set("Vary", "Cookie")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// dropDraft убирает черновик, ушедший в заметку. Отдельным методом, потому что
// зовут его два обработчика, а склада может не быть вовсе (перекодировщик не
// поднялся) — проверка эта не должна повторяться на каждой площадке вызова.
func (s *Server) dropDraft(id string) {
	if s.drafts != nil {
		s.drafts.drop(id)
	}
}
