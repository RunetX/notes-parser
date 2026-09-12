package web

// Статика и медиа.
//
// Имя файла статики несёт хеш содержимого (style.a1b2c3d4.css), поэтому его
// можно отдавать с immutable на год и никогда не думать о сбросе кэша: новая
// сборка — новое имя. Тот же приём, что у хранилища медиа, только там имя есть
// сам sha256 файла.
//
// Подкаталоги тоже статика: в assets/smile лежат картинки смайлов НГС
// (smiles.go), в assets/profile — силуэты, которые сайт показывает вместо
// пустого фото (silhouette.go), в assets/font — два шрифта темы «Гримуар».
// Они вшиты в бинарник, а не сложены в хранилище медиа, потому что это не чьё-то
// вложение, а часть ВИДА страницы — как иконка.
//
// ШРИФТЫ ЛЕЖАТ У НАС, а не берутся с fonts.googleapis.com, и причин две: строгий
// CSP чужой шрифтовый хост не пускает, а запрос к нему рассказывал бы
// постороннему сервису, кто и когда открыл страницу. Оба — SIL OFL, лицензии
// лежат рядом файлами (этого требует сама лицензия), и получены они так:
//
//	curl -LO https://raw.githubusercontent.com/google/fonts/main/ofl/ebgaramond/EBGaramond%5Bwght%5D.ttf
//	curl -LO https://raw.githubusercontent.com/google/fonts/main/ofl/ibmplexmono/IBMPlexMono-Regular.ttf
//	python -m fontTools.varLib.instancer 'EBGaramond[wght].ttf' wght=500 -o EBGaramond-500.ttf
//	python -m fontTools.subset <файл> --flavor=woff2 --no-hinting --desubroutinize \
//	  --layout-features=kern,liga,calt,locl,ccmp,mark,mkmk \
//	  --unicodes=U+0000-00FF,U+0131,U+0152-0153,U+02BB-02BC,U+02C6,U+02DA,U+02DC,\
//	U+0301,U+0304,U+0308,U+0329,U+0400-045F,U+0490-0491,U+04B0-04B1,U+2000-206F,\
//	U+20AC,U+2116,U+2122,U+2190-2193,U+2212,U+2215,U+FEFF,U+FFFD
//
// Подрезка до кириллицы и латиницы — не экономия ради экономии: целиком EB
// Garamond весит полмегабайта, а качает его КАЖДЫЙ гость (светлая сестра
// «Гримуара» стои́т темой по умолчанию). После подрезки пара весит 43 КБ.

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
)

//go:embed assets
var assetFS embed.FS

// immutableCache — год и immutable. Оправдано ровно потому, что адрес меняется
// вместе с содержимым: и у статики (хеш в имени), и у медиа (имя = sha256).
const immutableCache = "public, max-age=31536000, immutable"

type asset struct {
	data []byte
	mime string
	etag string
}

var (
	assets    = map[string]asset{}  // хешированное имя → файл
	assetURLs = map[string]string{} // исходное имя → путь с хешем
)

// assetRefRe — ссылка на соседний файл статики в тексте CSS: url("/assets/…").
// Точка входа ровно одна, поэтому и выражение простое: адрес без хеша в CSS
// бывает только написанный рукой.
var assetRefRe = regexp.MustCompile(`/assets/[A-Za-z0-9_./-]+`)

func init() {
	var names []string
	if err := fs.WalkDir(assetFS, "assets", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, p)
		}
		return err
	}); err != nil || len(names) == 0 {
		panic("web: статика не найдена")
	}
	// Проход ДВА, и порядок между ними обязателен: CSS ссылается на соседние
	// файлы (@font-face), а адрес у них с хешем, — значит подставить его можно
	// только после того, как посчитан хеш всего остального. Хеш самого стиля
	// считается уже от подставленного текста, поэтому смена шрифта меняет и
	// адрес style.css: иначе у половины читателей осталась бы старая пара
	// «стиль в кэше — шрифт, которого больше нет».
	for _, n := range names {
		if path.Ext(n) != ".css" {
			addAsset(n, mustReadAsset(n))
		}
	}
	for _, n := range names {
		if path.Ext(n) == ".css" {
			addAsset(n, resolveAssetRefs(n, mustReadAsset(n)))
		}
	}
	initSmiles()
}

func mustReadAsset(n string) []byte {
	data, err := assetFS.ReadFile(n)
	if err != nil {
		panic("web: не читается " + n)
	}
	return data
}

func addAsset(n string, data []byte) {
	sum := sha256.Sum256(data)
	h := hex.EncodeToString(sum[:])[:8]
	// Имя внутри assets/ сохраняется целиком (smile/popcorn.gif): подкаталог
	// виден в адресе, и одинаковые имена в разных папках не столкнутся.
	name := strings.TrimPrefix(n, "assets/")
	ext := path.Ext(name)
	hashed := strings.TrimSuffix(name, ext) + "." + h + ext
	assets[hashed] = asset{data: data, mime: assetMIME(ext), etag: `"` + h + `"`}
	assetURLs[name] = "/assets/" + hashed
}

// resolveAssetRefs подставляет в CSS настоящие адреса соседних файлов.
//
// Неизвестное имя — ПАНИКА, а не пустая ссылка: опечатка в url() дала бы
// молчаливый 404 на шрифт, то есть страницу, которая выглядит «почти как надо»
// и потому проходит глазами. Тем же init'ом мы уже падаем на нечитаемом файле.
func resolveAssetRefs(n string, data []byte) []byte {
	return assetRefRe.ReplaceAllFunc(data, func(ref []byte) []byte {
		name := strings.TrimPrefix(string(ref), "/assets/")
		url, ok := assetURLs[name]
		if !ok {
			panic("web: " + n + " ссылается на " + string(ref) + ", а такого файла в статике нет")
		}
		return []byte(url)
	})
}

// assetURL — путь файла для шаблона. Отсутствующее имя даёт пустую ссылку, а не
// панику: сломанный css лучше пятисотки на всех страницах сразу.
func assetURL(name string) string { return assetURLs[name] }

func assetMIME(ext string) string {
	switch ext {
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".gif":
		return "image/gif"
	case ".png":
		return "image/png"
	case ".woff2":
		return "font/woff2"
	case ".txt":
		// Лицензии шрифтов. Текстом, а не файлом на скачивание: лицензия обязана
		// ехать со шрифтом, и прочесть её проще по ссылке, чем сохранив.
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	a, ok := assets[r.PathValue("name")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", a.mime)
	h.Set("Cache-Control", immutableCache)
	h.Set("ETag", a.etag)
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, a.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(a.data)
}

// mediaServer отдаёт файлы хранилища.
//
// В бою этот путь перехватывает Caddy и до Go он не доходит: на одном ядре
// самый жирный по трафику путь не должен проходить через приложение. Обработчик
// нужен разработке — и он же честно повторяет решение Caddy не спрашивать
// входа: адрес файла есть его sha256, то есть 256 бит, которые нельзя ни
// подобрать, ни перечислить. Ссылку на медиа надо сперва получить со страницы,
// а страницы за воротами.
type mediaServer struct {
	root *os.Root
}

func newMediaServer(dir string) (*mediaServer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// os.Root вместо склейки путей: выход за каталог невозможен по устройству,
	// а не потому, что мы вспомнили про «..».
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &mediaServer{root: root}, nil
}

func (m *mediaServer) Close() error {
	if m == nil || m.root == nil {
		return nil
	}
	return m.root.Close()
}

func (m *mediaServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" || strings.HasSuffix(name, "/") || !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}
	f, err := m.root.Open(name)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			http.Error(w, "ошибка чтения", http.StatusInternalServerError)
			return
		}
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil || st.IsDir() {
		// Каталог — не «пустой ответ», а именно 404: списка файлов хранилища
		// наружу быть не должно.
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Cache-Control", immutableCache)
	// Тип определяется по расширению, а его хранилище ставит по содержимому
	// файла, а не по ссылке. nosniff здесь же: «аватар», оказавшийся html, не
	// должен исполниться на нашем домене.
	h.Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, name, st.ModTime().UTC().Truncate(time.Second), f)
}
