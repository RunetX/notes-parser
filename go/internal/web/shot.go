package web

// Приём картинки, приложенной к заметке. Первый и единственный случай, когда
// файл площадке кладёт ПОСТОРОННИЙ, поэтому порядок проверок здесь — это
// порядок их цены, а не порядок удобства чтения.
//
// Перекодированием занимается imgconv (то есть отдельный процесс ffmpeg), морда
// же отвечает за три вещи: не пустить сюда того, кому всё равно откажут; не дать
// чужому файлу съесть память контейнера; и честно сказать человеку, что
// случилось, не потеряв набранный текст.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"lovegw/internal/imgconv"
)

const (
	// shotField — имя поля формы.
	shotField = "shot"

	// shotsInFlight — сколько картинок площадка держит в работе разом.
	//
	// ДВЕ, и число это про память, а не про процессор: на одну закачку
	// приходится около тридцати мегабайт пиковых (тело запроса, буфер разбора
	// multipart и копия для ffmpeg), а общий семафор морды пускает двенадцать
	// запросов — двенадцать закачек это триста шестьдесят мегабайт при жёстком
	// потолке контейнера в триста двадцать.
	//
	// Тем же слотом ограничено и число одновременных ffmpeg: второй семафор
	// охранял бы то, что уже охраняется.
	shotsInFlight = 2

	// shotsWait — сколько ждём слот. Мгновенный отказ при двух слотах
	// срабатывал бы на честной паре одновременных публикаций.
	shotsWait = 3 * time.Second
)

// Shot — картинка, приложенная к заметке: УЖЕ перекодированные байты и размеры.
//
// Размеры здесь потому, что задавали их мы сами, а stdlib не умеет читать webp
// и посчитать их заново не сможет.
type Shot struct {
	Data   []byte
	Width  int
	Height int
}

// SetShots включает приём картинок. nil (или не позванный вовсе) означает
// «картинок площадка не принимает»: поля файла на форме тогда нет вовсе —
// кнопка, ведущая к гарантированному отказу, хуже её отсутствия.
func (s *Server) SetShots(c imgconv.Converter) {
	if c == nil || c.Codec() == "" {
		return
	}
	s.shots = c
	s.shotSem = make(chan struct{}, shotsInFlight)
}

// takesShots — принимаем ли мы сейчас файлы. Спрашивают шаблон формы и роутер.
func (s *Server) takesShots() bool { return s.shots != nil }

// postUpload — вход для ЕДИНСТВЕННОЙ формы, принимающей файл.
//
// Отдельно от postWrite не по вкусу, а по устройству net/http: ParseForm на
// multipart выходит БЕЗ ошибки, но оставляет r.Form пустой картой, после чего
// FormValue тело уже не разбирает — и checkCSRF гарантированно не находит
// токен. То есть общий вход здесь не «менее удобен», а сломан молча.
//
// Порядок: происхождение читается из заголовков и не стоит ничего, поэтому
// чужая страница получает отказ, не прислав ни байта тела.
func (s *Server) postUpload(w http.ResponseWriter, r *http.Request) bool {
	if !sameOrigin(r) {
		s.fail(w, r, http.StatusForbidden, "Запрос пришёл не с нашей страницы.")
		return false
	}
	if err := r.ParseMultipartForm(uploadMaxBytes); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.fail(w, r, http.StatusRequestEntityTooLarge,
				"Отправленное больше 10 МБ. Столько площадка не примет: файлы лежат на том же диске, что и вся переписка.")
			return false
		}
		s.fail(w, r, http.StatusBadRequest, "Форма не разобралась.")
		return false
	}
	return s.checkCSRF(w, r)
}

// isMultipart — форма пришла с файлом. Прежняя дорога (urlencoded) остаётся
// рабочей: без JS, из старой вкладки и вообще всегда, когда картинки нет.
func isMultipart(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data")
}

// takeShot достаёт приложенный файл и приводит его к тому, что мы храним.
//
// Три исхода: файла не было (nil, ""), файл негоден (nil, текст для человека),
// готово (картинка, ""). Поломки уходят в третий вид отказа — с текстом, а не
// пятисоткой: чужой файл, который не понравился ffmpeg, это не наша авария.
func (s *Server) takeShot(ctx context.Context, r *http.Request) (*Shot, string) {
	if s.shots == nil || r.MultipartForm == nil {
		return nil, ""
	}
	f, hdr, err := r.FormFile(shotField)
	if err != nil {
		return nil, "" // поля нет или файл не выбран — обычный случай
	}
	defer f.Close()
	if hdr.Size == 0 {
		return nil, ""
	}
	if hdr.Size > imgconv.MaxBytes {
		return nil, "Картинка больше 10 МБ. Уменьшите её и попробуйте снова."
	}

	// Размер известен из заголовка части, и потолок уже проверен, поэтому
	// читаем ровно столько: ReadAll на чужом файле — это отказ от потолка.
	data := make([]byte, hdr.Size)
	if _, err := io.ReadFull(f, data); err != nil {
		return nil, "Файл не дочитался. Попробуйте отправить ещё раз."
	}

	res, err := s.shots.Convert(ctx, data)
	if err != nil {
		return nil, shotProblem(err)
	}
	return &Shot{Data: res.Data, Width: res.Width, Height: res.Height}, ""
}

// shotProblem переводит отказ перекодировщика в текст для человека. Числа в нём
// названы вслух: «что-то пошло не так» не говорит, что делать дальше.
func shotProblem(err error) string {
	switch {
	case errors.Is(err, imgconv.ErrTooBig):
		return "Картинка больше 10 МБ. Уменьшите её и попробуйте снова."
	case errors.Is(err, imgconv.ErrNotImage):
		return "Это не картинка. Площадка принимает JPEG, PNG, GIF и WebP."
	case errors.Is(err, imgconv.ErrTooManyPx):
		return "В картинке слишком много точек: предел — 24 миллиона, это больше любого телефонного снимка. " +
			"Уменьшите разрешение и попробуйте снова."
	default:
		return "Картинка сейчас не обработалась. Попробуйте ещё раз через минуту или опубликуйте заметку без неё."
	}
}

// takeShotSlot занимает место в очереди перекодирования.
//
// Слот берётся ДО чтения тела, а не перед запуском ffmpeg: память ест не
// перекодирование, а само тело запроса — десять мегабайт, которые к моменту
// вызова ffmpeg уже лежат у нас дважды.
func (s *Server) takeShotSlot(w http.ResponseWriter, r *http.Request) (release func(), ok bool) {
	if s.shotSem == nil || !isMultipart(r) {
		return func() {}, true
	}
	t := time.NewTimer(shotsWait)
	defer t.Stop()
	select {
	case s.shotSem <- struct{}{}:
		return func() { <-s.shotSem }, true
	case <-r.Context().Done():
		return nil, false
	case <-t.C:
		retryLater(w, http.StatusServiceUnavailable, 5,
			"Площадка сейчас принимает другую картинку. Подождите несколько секунд и отправьте снова.")
		return nil, false
	}
}
