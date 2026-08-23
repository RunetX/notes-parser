package web

// Превью ролика: как оно попадает к нам на диск и почему лежит именно так.
//
// Файл кладётся В КАТАЛОГ МЕДИА, но НЕ в CAS и не в таблицу media, и это
// осознанное исключение из общего правила. Файл в CAS адресуется хешем
// СОДЕРЖИМОГО, поэтому страница обязана спросить у базы, какой именно хеш ей
// рисовать, — а здесь имя выводится из ссылки, которая и так лежит в тексте.
// Значит показу не нужен ни запрос, ни колонка, ни миграция, ни строчка в
// интерфейсе Store: карточку рисует тот же шаблон, что и всё остальное.
// Плата — файл, которого нет в media. Она безопасна ровно потому, что уборки
// каталога у площадки нет вовсе, а потерянное превью восстанавливается само:
// оно выводится из ссылки.
//
// Тянется превью ЛЕНИВО и в фоне: показ его только СПРАШИВАЕТ (os.Stat), но
// никогда не ждёт. Отсюда следствие, которое надо знать, читая это место:
// первый читатель новой ссылки видит текст, а карточку — второй. Сделано так
// потому, что показ страницы не имеет права зависеть от чужого хоста — YouTube
// в России замедлен, и синхронный поход за картинкой означал бы тред, который
// грузится столько, сколько сегодня отвечает Google. Публикация ту же закачку
// подталкивает сама (videoWarm в write.go), так что к перезагрузке страницы
// автором превью обычно уже лежит.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// previewDir — подкаталог медиа под превью. Одна буква не пересекается с
	// раскладкой CAS: та кладёт файлы в каталоги из ДВУХ шестнадцатеричных
	// знаков.
	previewDir = "v"
	previewExt = ".jpg"

	// previewTimeout — сколько ждём чужой хост. Щедро, потому что ждёт фоновая
	// горутина, а не читатель.
	previewTimeout = 20 * time.Second

	// previewMaxBytes — потолок картинки. Превью — это 20–60 КБ; мегабайт с
	// чужого хоста означает, что нам отдали не то.
	previewMaxBytes = 4 << 20

	// previewRetryAfter — как долго не возвращаться к ссылке, на которой
	// сорвалось. Ролик сносят навсегда, а сеть чинится, поэтому срок средний:
	// без него каждый показ страницы стучался бы в чужой хост заново.
	previewRetryAfter = 6 * time.Hour
)

// previews — хранилище превью. Пакетная переменная по той же причине, что и
// ownLinkPrefix: шаблоны с их FuncMap разбираются один раз на процесс, а сервер
// в процессе один. Пусто — карточек нет вовсе, и все ссылки остаются текстом.
var previews *previewStore

type previewStore struct {
	dir    string
	client *http.Client
	log    *slog.Logger

	// present — что уже лежит на диске. Только положительные ответы: превью не
	// исчезает, поэтому запомненное «есть» не устаревает никогда, а
	// запоминать «нет» нельзя — оно перестаёт быть правдой через минуту.
	present sync.Map // key -> struct{}

	// pending — ссылки, за которыми прямо сейчас идём либо на которых недавно
	// сорвалось. Одно поле на два состояния намеренно: обоим нужен один ответ
	// «сюда сейчас не ходить», и различать их значило бы завести вторую карту
	// ради того же вывода.
	pending sync.Map // key -> time.Time (до какого момента не трогать)
}

// setVideoPreviews поднимает хранилище. Пустой каталог медиа значит «превью
// класть некуда» — тогда карточек нет и страница показывает ссылки текстом, как
// показывала до этой правки.
func setVideoPreviews(mediaDir string, log *slog.Logger) {
	if mediaDir == "" {
		previews = nil
		return
	}
	previews = &previewStore{
		dir:    filepath.Join(mediaDir, previewDir),
		client: &http.Client{Timeout: previewTimeout},
		log:    log,
	}
}

// has — лежит ли превью у нас. Заодно подталкивает закачку, если не лежит:
// другого места, которое знает, что эта ссылка кому-то показалась, нет.
func (s *previewStore) has(r videoRef) bool {
	key := r.key()
	if _, ok := s.present.Load(key); ok {
		return true
	}
	if _, err := os.Stat(s.path(r)); err == nil {
		s.present.Store(key, struct{}{})
		return true
	}
	s.kick(r)
	return false
}

// kick ставит закачку в фон, если за этой ссылкой сейчас не идут и недавно на
// ней не сорвались.
func (s *previewStore) kick(r videoRef) {
	key := r.key()
	now := time.Now()
	if until, loaded := s.pending.LoadOrStore(key, now.Add(previewTimeout)); loaded {
		if now.Before(until.(time.Time)) {
			return
		}
		// Срок вышел: либо прошлая попытка давно сорвалась, либо горутина
		// умерла вместе с процессом. Пробуем снова, заняв ссылку заново.
		if !s.pending.CompareAndSwap(key, until, now.Add(previewTimeout)) {
			return
		}
	}
	go func() {
		if err := s.fetch(r); err != nil {
			// Не warn: недоступный чужой хост и снесённый ролик — это не наша
			// поломка, а обычное состояние мира, и ссылка от этого просто
			// остаётся текстом.
			s.log.Debug("превью ролика не забрано", "video", key, "err", err)
			s.pending.Store(key, time.Now().Add(previewRetryAfter))
			return
		}
		s.present.Store(key, struct{}{})
		s.pending.Delete(key)
		s.log.Info("превью ролика забрано", "video", key)
	}()
}

// fetch забирает картинку и кладёт её на диск.
func (s *previewStore) fetch(r videoRef) error {
	ctx, cancel := context.WithTimeout(context.Background(), previewTimeout)
	defer cancel()

	src := ""
	if r.p.thumb != nil {
		src = r.p.thumb(r.id)
	} else {
		var err error
		if src, err = s.askOEmbed(ctx, r); err != nil {
			return err
		}
	}
	data, err := s.get(ctx, src)
	if err != nil {
		return err
	}
	// Проверка типа — не придирка: DDoS-Guard и чужие заглушки отдают на запрос
	// картинки HTML-страницу с кодом 200, и такое «превью» осело бы на диске
	// молча, а на странице появилось битым. Тот же довод — в platform.MediaStore.Put.
	if mime := http.DetectContentType(data); !strings.HasPrefix(mime, "image/") {
		return &previewError{what: "не картинка, а " + mime, url: src}
	}
	return s.write(r, data)
}

// askOEmbed спрашивает у площадки адрес превью. Нужен там, где он не выводится
// из идентификатора — то есть везде, кроме YouTube.
func (s *previewStore) askOEmbed(ctx context.Context, r videoRef) (string, error) {
	data, err := s.get(ctx, r.p.oembed(r.page))
	if err != nil {
		return "", err
	}
	var doc struct {
		Thumbnail string `json:"thumbnail_url"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", &previewError{what: "oEmbed не разобран", url: r.page}
	}
	if !strings.HasPrefix(doc.Thumbnail, "https://") {
		return "", &previewError{what: "oEmbed без превью", url: r.page}
	}
	return doc.Thumbnail, nil
}

func (s *previewStore) get(ctx context.Context, addr string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &previewError{what: "код " + resp.Status, url: addr}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, previewMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, &previewError{what: "пусто", url: addr}
	}
	if len(data) > previewMaxBytes {
		return nil, &previewError{what: "больше потолка", url: addr}
	}
	return data, nil
}

// write кладёт байты на диск через временный файл и rename — иначе оборванная
// закачка оставила бы обрезанную картинку под правильным именем, то есть
// навсегда. Тот же приём, что у MediaStore.write, и по той же причине.
func (s *previewStore) write(r videoRef, data []byte) error {
	path := s.path(r)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // после удачного rename это no-op

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Читать файл будет Caddy из-под своего пользователя, а не мы.
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// path — где лежит превью. Собирается из полей, каждое из которых проверено:
// каталог берётся из закрытого списка площадок, идентификатор — из регулярки
// этой площадки, так что выйти за каталог отсюда нечем.
func (s *previewStore) path(r videoRef) string {
	return filepath.Join(s.dir, r.p.dir, r.id+previewExt)
}

// videoWarm — подтолкнуть закачку превью для только что опубликованного текста.
// Зовётся с публикации: свою заметку автор перечитывает сразу, и ждать, пока
// карточку «откроет» второй читатель, ему незачем.
//
// Ответ has здесь и не нужен, и не смотрится: важен его побочный эффект —
// закачка в фон. Публикация от неё не зависит и на её отказе не спотыкается.
func videoWarm(id int64, body string) {
	for _, r := range videoRefs(id, body) {
		previews.has(r)
	}
}

// previewError — почему превью не приехало. Свой тип, чтобы в логе стояла и
// причина, и адрес: без адреса строка «код 404» ничего не значит.
type previewError struct{ what, url string }

func (e *previewError) Error() string { return e.what + " (" + e.url + ")" }
