// Пакет platsink — собственная площадка как третий приёмник зеркала
// (mirror.Sink рядом с Telegram и MAX). Мессенджером она при этом не является:
// ЛС никому не пишет, подписчиков не уведомляет, читатель приходит сам.
//
// Что площадка получает от зеркала даром:
//
//   - байты медиа — аватары и иллюстрации приезжают уже скачанными с RU-IP
//     (Telegram их с love.ngs.ru забрать не может, поэтому качает сам демон),
//     так что хранилище наполняется без единого лишнего запроса к сайту;
//   - адресата реплики — зеркало уже сопоставило обращение «Ник, …» с
//     сообщением приёмника, а «сообщение приёмника» у площадки это id нашего же
//     комментария. Считать адресата второй раз не нужно.
//
// Чего у зеркала нет и что достраивается потом: корень ветки и настоящее дерево
// ответов — их поднимает reply_scan по мобильной версии сайта (Ш6).
//
// Вторая нога приёма — сверка (reconcile.go). Живой приёмник обязан быть у
// зеркала честным: его ошибка оставляет заметку неотправленной и тормозит
// ОСТАЛЬНЫЕ приёмники тоже (mirror ставит posted, только когда отработали все).
// Поэтому лежащий Postgres чинится не ретраями внутри, а сверкой снаружи.
package platsink

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"lovegw/internal/love"
	"lovegw/internal/platform"
	"lovegw/internal/store"
)

// Name — имя приёмника в message_targets. Колонка messenger там свободный TEXT
// без CHECK, поэтому новый приёмник встраивается без миграции SQLite.
const Name = store.MessengerPlatform

// opBudget — потолок одного обращения к площадке из потока зеркала.
//
// Он здесь не ради Postgres, а ради КАНАЛОВ. Приёмники зеркало обходит подряд,
// одной горутиной на заметку, и вызов без срока держит эту горутину столько,
// сколько занята база: наплыв на веб-морду (единственное, до чего дотягивается
// посторонний) превращался бы в молчание в Telegram и MAX — при том что тем
// двоим база не нужна вовсе. Пять секунд при приёме в один INSERT по первичному
// ключу — это «база занята», а не «база думает».
//
// Отказ по сроку безопасен и предусмотрен устройством: заметка остаётся
// неотправленной, следующий цикл повторит, а не повторившееся догонит сверка.
// Ждать здесь дольше нечего — писать всё равно некуда.
const opBudget = 5 * time.Second

// Sink принимает поток зеркала в Postgres площадки.
type Sink struct {
	p     *platform.Platform
	media *platform.MediaStore
	log   *slog.Logger
}

// New создаёт приёмник. media обязателен: без хранилища аватары и иллюстрации
// остались бы ссылками на hsmedia.ru, а страницы площадки не должны ходить на
// чужой домен вовсе.
func New(p *platform.Platform, media *platform.MediaStore, log *slog.Logger) *Sink {
	if log == nil {
		log = slog.Default()
	}
	return &Sink{p: p, media: media, log: log}
}

func (s *Sink) Name() string { return Name }

// PostNote принимает заметку. Возвращает её id: для message_targets это
// «сообщение приёмника», и у площадки им честно является сама заметка.
func (s *Sink) PostNote(ctx context.Context, n store.Note, avatar []byte) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, opBudget)
	defer cancel()
	in, err := noteFrom(n)
	if err != nil {
		return "", err
	}
	fresh, err := s.p.IngestNote(ctx, in)
	if err != nil {
		return "", err
	}
	s.putAvatar(ctx, in.Author.ID, n.AuthorAvatarURL, avatar)
	if fresh {
		s.log.Info("заметка принята площадкой", "note", n.ID)
	}
	return n.ID, nil
}

// StartThread ничего не открывает: тред площадки — это сама страница заметки.
// Метод существует ради зеркала: приёмник без ThreadStarter остаётся без корня
// треда навсегда, а значит и без комментариев (sendUnsent его пропускает).
//
// Записей здесь нет намеренно: зеркало зовёт StartThread ДО PostNote, и заметки
// в базе на этот момент может ещё не быть.
func (s *Sink) StartThread(_ context.Context, n store.Note, _ string) (string, error) {
	return n.ID, nil
}

// PostComment принимает комментарий.
//
// Адресата площадка ищет САМА, а ответ зеркала (replyToID) не берёт вовсе, и
// это не пренебрежение: зеркало отвечает по своей памяти, а память эта — база
// НГС, где нативной реплики нет и быть не может. Тред у площадки СМЕШАННЫЙ с
// того дня, как здесь стало можно писать, и ответ «Ник, …», написанный на сайте
// в ответ на сказанное ЗДЕСЬ, зеркало приклеивало к последней реплике этого
// человека НА НГС — в чужую ветку, и починить это потом нечем: обход мобильного
// дерева нативной реплики тоже не видит (жалоба владельца 24.08.2026).
//
// Правило при этом ТО ЖЕ (platform.AddresseeInNote), поэтому живой приём и
// сверка по-прежнему сходятся на одном ответе.
func (s *Sink) PostComment(ctx context.Context, n store.Note, _, _ string,
	c store.Comment, avatar []byte) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, opBudget)
	defer cancel()
	noteID, err := siteID(n.ID)
	if err != nil {
		return "", err
	}
	replyTo, err := s.addressee(ctx, noteID, c)
	if err != nil {
		return "", err
	}
	in := commentFrom(noteID, c, replyTo)
	if _, err := s.p.IngestComment(ctx, in); err != nil {
		return "", err
	}
	s.putAvatar(ctx, in.Author.ID, c.AvatarURL, avatar)
	return strconv.FormatInt(c.ID, 10), nil
}

// addressee — кому отвечает реплика. Обращения нет — ноль, и реплика встаёт
// корнем ветки, как вставала до слоя адресатов.
func (s *Sink) addressee(ctx context.Context, noteID int64, c store.Comment) (int64, error) {
	nick := love.AddressPrefix(c.Text)
	if nick == "" {
		return 0, nil
	}
	// Граница — ровно то время, под которым реплика ляжет в базу (publishedAt),
	// а не время, когда её увидело зеркало: сравнивается она с published_at
	// соседей по треду, и две разные шкалы дали бы разный ответ у живого приёма
	// и у сверки.
	return s.p.AddresseeInNote(ctx, noteID, nick, c.AuthorName, publishedAt(c), c.ID)
}

// PostNoteImage кладёт иллюстрацию в хранилище и привязывает её к заметке.
// threadID у площадки — id заметки (см. StartThread).
func (s *Sink) PostNoteImage(ctx context.Context, threadID, imageURL string, image []byte) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, opBudget)
	defer cancel()
	noteID, err := siteID(threadID)
	if err != nil {
		return "", err
	}
	var (
		sha []byte
		url = imageURL
	)
	if len(image) > 0 {
		m, err := s.media.Put(ctx, image, imageURL)
		if err != nil {
			// Не отказ: ссылку сохраняем всё равно, байты доберём позже. Отказ
			// стоил бы очереди комментариев — зеркало не пускает их в тред
			// раньше картинки.
			s.log.Warn("иллюстрация не легла в хранилище", "note", noteID, "url", imageURL, "err", err)
		} else {
			sha, url = m.SHA256, m.URL
		}
	}
	if err := s.p.AttachNoteImage(ctx, noteID, sha, imageURL); err != nil {
		return "", err
	}
	return url, nil
}

// putAvatar сохраняет аватар автора. Ошибки здесь мягкие: без картинки страница
// жива, а отказ приёмника остановил бы очередь заметки во ВСЕХ мессенджерах.
//
// Силуэт по умолчанию не сохраняем: «аватар есть у всех» — это не аватар, а фон,
// и рисовать его должна разметка площадки, а не 23 тысячи ссылок на одну и ту же
// картинку НГС.
func (s *Sink) putAvatar(ctx context.Context, userID int64, url string, data []byte) {
	if userID == 0 || len(data) == 0 || !love.IsRealAvatar(url) {
		return
	}
	m, err := s.media.Put(ctx, data, url)
	if err != nil {
		s.log.Warn("аватар не лёг в хранилище", "user", userID, "url", url, "err", err)
		return
	}
	if err := s.p.SetAvatar(ctx, userID, m.SHA256); err != nil {
		s.log.Warn("аватар не привязан к человеку", "user", userID, "err", err)
	}
}

// siteID разбирает id сайта. Ошибка тут означает дрейф разметки (id заметок на
// НГС числовые), и молчать о ней нельзя: зеркало повторит заметку, а мы увидим
// причину в логе.
func siteID(id string) (int64, error) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("id заметки %q не число: %w", id, err)
	}
	if !platform.IsNGS(n) {
		return 0, fmt.Errorf("id заметки %d вне полосы НГС", n)
	}
	return n, nil
}

// replyID разбирает id адресата. Пусто — адресата нет, это рабочий случай.
func replyID(id string) (int64, error) {
	if id == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("id адресата %q не число: %w", id, err)
	}
	return n, nil
}
