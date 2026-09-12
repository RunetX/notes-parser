package platform

// Рассказ о себе и фотографии профиля (эпик M).
//
// Место, где человек говорит о себе, а не сервис знакомств: симпатий, гостей,
// поиска людей и «просьбы добавить фото» здесь нет и не заводится — каждая
// такая вещь есть новая категория данных и новый документ, а страница полезна
// и без них.
//
// ДВЕ ДВЕРИ К ОДНОЙ КОЛОНКЕ. users.bio заведена в 0019 жителю, и SetPersonaBio
// держит её проверкой `WHERE persona`. Эпик снимает не проверку, а
// исключительность: рядом встаёт SetAbout для ЖИВОГО человека, и у неё своё
// основание — подписанный документ. Это не дубль: у персонажа субъекта нет
// вовсе, и спрашивать у него согласие не у кого, а у человека нет оператора,
// который отвечал бы за его слова.
//
// СОГЛАСИЕ НЕОБЯЗАТЕЛЬНОЕ И СПРАШИВАЕТСЯ ПРИ ДЕЙСТВИИ — тот же рычаг, что у
// привязки мессенджера и у переписки. Абзац в общем документе означал бы новую
// редакцию `processing`, то есть подпись ЗАНОВО со всех до единого, включая
// тех, кто о себе рассказывать не собирался.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

var (
	// ErrNoProfileConsent — документ не подписан. Отдельно от ErrNotMember,
	// потому что ответ человеку разный: одному «вам сюда нельзя», другому «вот
	// документ, прочтите». Морда по этой ошибке ведёт к документу, а не прячет
	// форму: спрятанная кнопка ничего не объясняет.
	ErrNoProfileConsent = errors.New("согласие на рассказ о себе не подписано")
	// ErrPhotoLimit — все три места заняты. Потолок держит база (CHECK и
	// UNIQUE), эта ошибка лишь переводит её отказ на человеческий.
	ErrPhotoLimit = errors.New("больше трёх фотографий профиля не бывает")
	// ErrNoPhoto — такой фотографии у этого человека нет.
	ErrNoPhoto = errors.New("фотографии нет")
)

const (
	// MaxAboutRunes — потолок рассказа о себе. Вдесятеро меньше заметки
	// (MaxBodyRunes = 20000) и это не санитарная граница, а продуктовая:
	// страница отвечает на «кто это», и рассказ длиной в заметку отодвинул бы
	// вниз всё, ради чего на неё приходят. Порог свёртки показа
	// (web.longBodyRunes = 1500) лежит рядом и с этим числом не путается: там
	// решается, показать ли текст целиком, здесь — принять ли его вообще.
	MaxAboutRunes = 2000
	// MaxFactRunes — город и занятие. Строка справочной колонки, а не абзац:
	// «Новосибирск» и «слесарь-ремонтник» укладываются с запасом, а всё, что
	// длиннее, — это уже рассказ, и ему есть своё поле.
	MaxFactRunes = 64
	// PhotoLimit — сколько фотографий у человека. Три — решение владельца, и
	// второй довод к нему арифметический: каждая это строка в очередь человеку,
	// а картинки у нас судит ЧЕЛОВЕК. Число повторено здесь и в CHECK миграции
	// намеренно: база держит правило, а справка и формы обязаны его НАЗЫВАТЬ, и
	// спрашивать ради этого схему было бы дороже, чем сверить тестом.
	PhotoLimit = 3
)

// About — то, что человек рассказал о себе сам.
type About struct {
	Bio  string
	City string
	Job  string
}

// Photo — фотография профиля.
type Photo struct {
	ID       int64
	Position int
	URL      string
	Status   Status
}

// Hidden — скрыта модератором. Видна тогда только владельцу и модератору.
func (p Photo) Hidden() bool { return p.Status != StatusVisible }

// SetAbout — рассказ человека о себе.
//
// Пустые поля законны и означают «не рассказываю»: страница пропускает их
// поштучно, а строка «Город: —» хуже отсутствующей.
//
// Той же транзакцией карточка встаёт в очередь проверки — по тому же правилу,
// по которому туда встаёт заметка: «опубликовано, но в очередь не попало» не
// должно существовать. Вид объекта один на всю карточку (SubjectProfile), а не
// по строке на поле: модератор судит то, что видит, а видит он страницу.
func (p *Platform) SetAbout(ctx context.Context, userID int64, in About) error {
	bio, err := cleanAbout(in.Bio, MaxAboutRunes)
	if err != nil {
		return err
	}
	city, err := cleanAbout(in.City, MaxFactRunes)
	if err != nil {
		return err
	}
	job, err := cleanAbout(in.Job, MaxFactRunes)
	if err != nil {
		return err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("рассказ о себе %d: %w", userID, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	if err := aboutGuard(ctx, tx, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE users SET bio = $2, city = $3, job = $4 WHERE id = $1`,
		userID, bio, city, job); err != nil {
		return fmt.Errorf("рассказ о себе %d: %w", userID, err)
	}
	// Пустую карточку к человеку не отправляем: судить нечего, а строка в
	// очереди стоит его времени. Стереть всё — это ровно то же, что и не
	// заполнять, и проверять отсутствие рассказа не за чем.
	if bio != "" || city != "" || job != "" {
		if err := enqueueCheck(ctx, tx, SubjectProfile, userID, 0, userID); err != nil {
			return err
		}
	}
	return wrapf(tx.Commit(ctx), "рассказ о себе %d", userID)
}

// MayTellAbout — вправе ли человек сейчас что-то о себе рассказывать.
//
// Спрашивается ДО перекодирования фотографии, и это тот же порядок и тот же
// довод, что у MayPublishNote: отказ не должен стоить ни процессора, ни файла,
// который после отказа убирать будет некому — обхода каталога у площадки нет.
// Гонка в сто миллисекунд оставит один осиротевший файл, отсутствие проверки —
// столько, сколько успеет прислать отказавшийся верить.
func (p *Platform) MayTellAbout(ctx context.Context, userID int64) error {
	return aboutGuard(ctx, p.pool, userID)
}

// AddProfilePhoto кладёт фотографию на первое свободное место.
//
// Байты к этому моменту УЖЕ в хранилище (их кладёт вызывающий, тем же кодом,
// что у зеркала и у аватара), и порядок этот обязателен: user_photos ссылается
// на media, а строка без файла — это битая картинка на странице, то есть
// поломка видимая. Файл без строки не виден никому и убирается уборкой.
func (p *Platform) AddProfilePhoto(ctx context.Context, userID int64, m *Media) (int, error) {
	if m == nil || len(m.SHA256) == 0 {
		return 0, ErrNoPhoto
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("фотография профиля %d: %w", userID, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	if err := aboutGuard(ctx, tx, userID); err != nil {
		return 0, err
	}
	// Место ищется ЗАПРОСОМ и под той же транзакцией, что и вставка: считать
	// занятое в Go значило бы дать двум одновременным загрузкам одно место, а
	// уникальный ключ отказал бы второй непонятной человеку ошибкой.
	var pos int
	err = tx.QueryRow(ctx, `
		SELECT g.n FROM generate_series(1, $2) AS g(n)
		 WHERE NOT EXISTS (SELECT 1 FROM user_photos
		                    WHERE user_id = $1 AND position = g.n)
		 ORDER BY g.n LIMIT 1`, userID, PhotoLimit).Scan(&pos)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrPhotoLimit
	}
	if err != nil {
		return 0, fmt.Errorf("фотография профиля %d: %w", userID, err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO user_photos (user_id, position, sha256) VALUES ($1, $2, $3)`,
		userID, pos, m.SHA256); err != nil {
		return 0, fmt.Errorf("фотография профиля %d: %w", userID, err)
	}
	// Фотографию смотрит ЧЕЛОВЕК и всегда: автомат проверки текстовый и в
	// изображения не смотрит вовсе. Тем же правилом к модератору попадает
	// заметка с картинкой.
	if err := enqueueCheck(ctx, tx, SubjectProfile, userID, 0, userID); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("фотография профиля %d: %w", userID, err)
	}
	return pos, nil
}

// RemoveProfilePhoto снимает фотографию и уносит за собой БАЙТЫ.
//
// Это единственное место проекта, где хранилище чистится, и правило здесь
// обратно общему. Общее: имя файла есть его содержимое, «Убрать фото» снимает
// привязку, а байты остаются — для аватара из анкеты НГС это честно, он и так
// публичен на сайте. Для фотографии, которую человек принёс СЮДА и потом убрал,
// — нет: она лежит по открытому адресу, и «у кого есть ссылка, тот файл увидит»
// сказано прямо в подписанном им документе.
//
// Порядок строгий: строка в транзакции, файл — ПОСЛЕ коммита. Обратный порядок
// даёт битую картинку у чужой живой строки, и починить её нечем; этот —
// осиротевший файл, который безвреден (ссылок на него нет, а адрес это 256 бит)
// и убирается тем же кодом при следующем случае.
func (p *Platform) RemoveProfilePhoto(ctx context.Context, userID int64, position int) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("снятие фотографии %d: %w", userID, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	var sha []byte
	err = tx.QueryRow(ctx, `
		DELETE FROM user_photos WHERE user_id = $1 AND position = $2
		RETURNING sha256`, userID, position).Scan(&sha)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoPhoto
	}
	if err != nil {
		return fmt.Errorf("снятие фотографии %d: %w", userID, err)
	}
	orphans, err := dropUnusedMedia(ctx, tx, sha)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("снятие фотографии %d: %w", userID, err)
	}
	p.dropFiles(orphans)
	return nil
}

// aboutOf — что человек рассказал о себе. Нужна выгрузке субъекта: страница
// читает эти колонки вместе с остальной карточкой (profileQuery), а выгрузка
// идёт по своему пути и второго запроса ради трёх полей не жалеет.
func (p *Platform) aboutOf(ctx context.Context, userID int64) (About, error) {
	var a About
	err := p.pool.QueryRow(ctx,
		`SELECT bio, city, job FROM users WHERE id = $1`, userID).Scan(&a.Bio, &a.City, &a.Job)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, wrapf(err, "рассказ о себе %d", userID)
}

// ProfilePhotos — альбом человека. all=false отдаёт только видимое: скрытую
// модератором фотографию видят двое — он сам и её владелец.
func (p *Platform) ProfilePhotos(ctx context.Context, userID int64, all bool) ([]Photo, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT p.id, p.position, p.sha256, m.mime, p.status
		  FROM user_photos p JOIN media m ON m.sha256 = p.sha256
		 WHERE p.user_id = $1 AND ($2 OR p.status = 0)
		 ORDER BY p.position`, userID, all)
	if err != nil {
		return nil, fmt.Errorf("альбом %d: %w", userID, err)
	}
	defer rows.Close()

	var out []Photo
	for rows.Next() {
		var (
			ph   Photo
			sha  []byte
			mime string
		)
		if err := rows.Scan(&ph.ID, &ph.Position, &sha, &mime, &ph.Status); err != nil {
			return nil, fmt.Errorf("альбом %d: %w", userID, err)
		}
		ph.URL = MediaURL(sha, mime)
		out = append(out, ph)
	}
	return out, wrapf(rows.Err(), "альбом %d", userID)
}

// HidePhotoAsModerator скрывает или возвращает ОДНУ фотографию.
//
// Отдельно от вердикта очереди, и это не удобство: вердикт судит карточку
// целиком (рассказ, город, занятие и все снимки разом), а дурной бывает одна
// фотография из трёх — унести с ней текст значило бы наказать за то, чего не
// смотрели. Скрытие, а не удаление, по общему правилу площадки: решение
// модератора обратимо нажатием, а вернуть удалённую фотографию человеку
// неоткуда.
func (p *Platform) HidePhotoAsModerator(ctx context.Context, actor Viewer, photoID int64, hide bool, reason string) error {
	if !actor.CanModerate() {
		return ErrNotModerator
	}
	status := StatusVisible
	action := ActionRestore
	if hide {
		status, action = StatusHiddenMod, ActionHide
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("фотография %d: %w", photoID, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	var owner int64
	err = tx.QueryRow(ctx, `
		UPDATE user_photos SET status = $2 WHERE id = $1 RETURNING user_id`,
		photoID, status).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoPhoto
	}
	if err != nil {
		return fmt.Errorf("фотография %d: %w", photoID, err)
	}
	// В журнал идёт ВЛАДЕЛЕЦ, а не номер снимка: журнал отвечает на вопрос «что
	// сделали с этим человеком», и номер строки, которой через день не станет,
	// на него не отвечает. Сам номер лежит рядом подробностью.
	if err := audit(ctx, tx, actor.UserID, action, ProfileSubject(owner), map[string]any{
		"photo": photoID, "reason": reason,
	}); err != nil {
		return err
	}
	return wrapf(tx.Commit(ctx), "фотография %d", photoID)
}

// aboutGuard — кто вправе рассказывать о себе: участник, не забаненный и
// подписавший документ. Проверка стои́т В ЯДРЕ, а не в форме, по тому же доводу,
// что и publishGuard: дорог к этим колонкам будет больше одной, а второй список
// правил рядом с этим однажды разойдётся.
func aboutGuard(ctx context.Context, q querier, userID int64) error {
	if err := writeGuard(ctx, q, userID); err != nil {
		return err
	}
	var ok bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM consents
		                WHERE user_id = $1 AND kind = $2 AND revoked_at IS NULL)`,
		userID, ConsentProfile).Scan(&ok)
	if err != nil {
		return fmt.Errorf("согласие на рассказ о себе %d: %w", userID, err)
	}
	if !ok {
		return ErrNoProfileConsent
	}
	return nil
}

// dropAboutData снимает всё, что человек рассказал о себе, и возвращает файлы,
// на которые больше никто не ссылается.
//
// Зовётся ДВАЖДЫ и из двух разных мест: при отзыве этого согласия и при
// обезличивании. Общая функция, а не две похожие: обещание документа («убирает
// всё разом: город, занятие, текст и все фотографии вместе с их файлами») одно,
// и два его исполнения разошлись бы молча.
func dropAboutData(ctx context.Context, q querier, userID int64) ([]Media, error) {
	rows, err := q.Query(ctx, `
		DELETE FROM user_photos WHERE user_id = $1 RETURNING sha256`, userID)
	if err != nil {
		return nil, fmt.Errorf("снятие фотографий %d: %w", userID, err)
	}
	var shas [][]byte
	for rows.Next() {
		var sha []byte
		if err := rows.Scan(&sha); err != nil {
			rows.Close()
			return nil, fmt.Errorf("снятие фотографий %d: %w", userID, err)
		}
		shas = append(shas, sha)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("снятие фотографий %d: %w", userID, err)
	}
	var orphans []Media
	for _, sha := range shas {
		got, err := dropUnusedMedia(ctx, q, sha)
		if err != nil {
			return nil, err
		}
		orphans = append(orphans, got...)
	}
	if _, err := q.Exec(ctx, `
		UPDATE users SET bio = '', city = '', job = '', about_status = 0
		 WHERE id = $1`, userID); err != nil {
		return nil, fmt.Errorf("снятие рассказа о себе %d: %w", userID, err)
	}
	return orphans, nil
}

// dropUnusedMedia убирает строку хранилища, если на эти байты больше не
// ссылается НИЧТО.
//
// Сверка обязательна и перечисляет все три места разом: имя файла есть его
// содержимое, поэтому двое вправе поставить одну и ту же картинку, и «моя
// фотография» и «чужой аватар» бывают одним файлом. Четвёртое место заведут
// через год — и пропущенная ссылка сделает уборку разрушительной, поэтому
// список стои́т здесь один, а не по месту вызова.
func dropUnusedMedia(ctx context.Context, q querier, sha []byte) ([]Media, error) {
	rows, err := q.Query(ctx, `
		DELETE FROM media WHERE sha256 = $1
		   AND NOT EXISTS (SELECT 1 FROM users       WHERE avatar_sha = $1)
		   AND NOT EXISTS (SELECT 1 FROM note_images WHERE sha256     = $1)
		   AND NOT EXISTS (SELECT 1 FROM user_photos WHERE sha256     = $1)
		RETURNING sha256, mime`, sha)
	if err != nil {
		return nil, fmt.Errorf("уборка медиа: %w", err)
	}
	defer rows.Close()

	var out []Media
	for rows.Next() {
		var m Media
		if err := rows.Scan(&m.SHA256, &m.MIME); err != nil {
			return nil, fmt.Errorf("уборка медиа: %w", err)
		}
		out = append(out, m)
	}
	return out, wrapf(rows.Err(), "уборка медиа")
}

// dropFiles сносит файлы осиротевших записей. Зовётся ПОСЛЕ коммита: файловая
// операция не откатывается вместе с транзакцией, и порядок «сперва база» —
// единственный, при котором сбой оставляет мусор, а не битую ссылку.
//
// Хранилища может не быть вовсе (разовая команда, тест) — тогда строки убраны,
// а файлы останутся сиротами: ссылок на них нет, и вреда от них тоже.
func (p *Platform) dropFiles(orphans []Media) {
	if p.media == nil {
		return
	}
	for _, m := range orphans {
		path := p.media.FilePath(m.SHA256, m.MIME)
		// Отказ не возвращается вызывающему НАМЕРЕННО: строки в базе уже нет,
		// действие человека совершилось, и «не удалось снять фотографию» было бы
		// про него неправдой. Цена названа прямо: файл, на который никто не
		// ссылается, останется лежать. Он безвреден — адрес есть sha256, и
		// ссылок на него нет, — но диск от таких растёт молча: обхода каталога
		// у площадки нет вовсе, и это долг, названный вслух, а не недосмотр.
		_ = os.Remove(path)
	}
}

func cleanAbout(s string, max int) (string, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\x00", ""))
	if utf8.RuneCountInString(s) > max {
		return "", ErrTooLong
	}
	return s, nil
}
