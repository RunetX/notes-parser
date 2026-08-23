package platform

// Права субъекта: выгрузка и обезличивание.
//
// Оба действия администраторские и живут в командной строке, а не кнопкой на
// странице, и причина у этого одна на двоих — ЦЕНА ПРОХОДА. Любая операция над
// «всеми публикациями одного человека» стоит десятки секунд (см. visibility.go),
// то есть в срок веб-запроса не влезает вовсе, а обезличивание вдобавок
// необратимо: подтверждение здесь нужно не галочкой, а перепиской. Закон это
// допускает прямо — на требование субъекта у оператора тридцать дней, а не
// секунда; самообслуживанием обязан работать ОТЗЫВ СОГЛАСИЯ, и он им и работает
// (кнопка на /me, исполняется немедленно).
//
// Что здесь важно понять про обезличивание. Стереть публикации нельзя: тред —
// это чужие ответы на чьи-то слова, и дыра в ветке ломает разговор посторонним,
// которые ничего не просили. Поэтому уходит не текст, а ЛИЧНОСТЬ: все строки
// человека переезжают на свежую нативную анкету-«могилу» без имени, а связь
// прежнего ряда с анкетой НГС рвётся. Могила своя на каждого — иначе разговоры
// разных людей слились бы в один голос, и тред стал бы враньём; при этом
// определить, чья она, без нашей помощи нельзя, а помощи не будет: соответствие
// «кто → какая могила» нигде не записывается, включая журнал.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// GraveNick — подпись обезличенного. Показывается под его прежними репликами и
// в обращениях к нему в чужих ответах (те рисуются из ТЕКУЩЕГО ника, поэтому
// достаточно поменять одну строку — это и было заложено с первой миграции).
const GraveNick = "Удалённый участник"

// AnonymizeResult — что переехало.
type AnonymizeResult struct {
	Notes     int
	Comments  int
	Reactions int
}

// AnonymizeUser исполняет требование субъекта об обезличивании.
//
// Одной транзакцией, и это осознанная плата: проход по автору с сотней тысяч
// реплик держит строки под замком минуту. Половинчатое обезличивание — часть
// строк переехала, часть нет — состояние, из которого нет хорошего выхода, а
// команда идёт в известный момент и под присмотром администратора.
func (p *Platform) AnonymizeUser(ctx context.Context, actor Viewer, userID int64) (AnonymizeResult, error) {
	var res AnonymizeResult
	u, err := p.UserByID(ctx, userID)
	if err != nil {
		return res, err
	}
	if u.AnonymizedAt != nil {
		return res, ErrAnonymized
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return res, wrapf(err, "обезличивание %d", userID)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	// Могила — обычная нативная строка users, помеченная обезличенной сразу:
	// так её не тронут ни зеркало (ensureShadow обходит обезличенных), ни вход,
	// ни фоновые обходы.
	var grave int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO users (id, nick, kind, anonymized_at)
		VALUES (nextval('users_native_seq'), $1, $2, now())
		RETURNING id`, GraveNick, KindShadow).Scan(&grave); err != nil {
		return res, wrapf(err, "обезличивание %d", userID)
	}
	moved := func(sql string) (int, error) {
		tag, err := tx.Exec(ctx, sql, userID, grave)
		if err != nil {
			return 0, wrapf(err, "обезличивание %d", userID)
		}
		return int(tag.RowsAffected()), nil
	}
	if res.Notes, err = moved(`UPDATE notes SET author_id = $2 WHERE author_id = $1`); err != nil {
		return res, err
	}
	// author_display гасится заодно: это снимок ника безанкетного комментатора
	// зеркала, то есть ровно то имя, которое человек и просил убрать.
	if res.Comments, err = moved(
		`UPDATE comments SET author_id = $2, author_display = '' WHERE author_id = $1`); err != nil {
		return res, err
	}
	if res.Reactions, err = moved(`UPDATE reactions SET user_id = $2 WHERE user_id = $1`); err != nil {
		return res, err
	}
	// Карточки проверки едут следом: иначе очередь модератора продолжала бы
	// показывать, кому принадлежала скрытая реплика.
	if _, err := moved(`UPDATE moderation_queue SET author_id = $2 WHERE author_id = $1`); err != nil {
		return res, err
	}
	// Шина чистится, а не переезжает на могилу вместе с публикациями: событие
	// «X ответил Y» — это связь между двумя людьми, и перенеся её, мы своей же
	// рукой сохранили бы ту дополнительную информацию, отсутствие которой и
	// делает обезличивание обезличиванием.
	if err := dropUserEvents(ctx, tx, userID); err != nil {
		return res, err
	}
	// Связь с анкетой НГС рвётся насовсем. Именно она и делает данные
	// «принадлежащими субъекту»: id строки равен номеру анкеты, и пока
	// identities на месте, обезличивание было бы косметикой.
	if _, err := tx.Exec(ctx, `DELETE FROM identities WHERE user_id = $1`, userID); err != nil {
		return res, wrapf(err, "обезличивание %d", userID)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE consents SET revoked_at = now()
		 WHERE user_id = $1 AND revoked_at IS NULL`, userID); err != nil {
		return res, wrapf(err, "обезличивание %d", userID)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE web_sessions SET revoked_at = now()
		 WHERE user_id = $1 AND revoked_at IS NULL`, userID); err != nil {
		return res, wrapf(err, "обезличивание %d", userID)
	}
	// Прежний ряд остаётся пустым: публикаций у него больше нет, имени и фото
	// тоже. Удалять его нельзя — на него смотрят внешние ключи журнала и
	// жалоб, а журнал модерации обязан пережить эту операцию.
	if _, err := tx.Exec(ctx, `
		UPDATE users
		   SET nick = '', avatar_sha = NULL, ngs_avatar_url = '', gender = 0,
		       kind = $2, hide_all = false, visibility_dirty = false,
		       ban_reason = '', anonymized_at = now()
		 WHERE id = $1`, userID, KindShadow); err != nil {
		return res, wrapf(err, "обезличивание %d", userID)
	}
	// В журнал идут ЧИСЛА, но не номер могилы. Записать его значило бы своей же
	// рукой сохранить ту «дополнительную информацию», отсутствие которой и
	// делает обезличивание обезличиванием.
	if err := audit(ctx, tx, actor.UserID, ActionAnonym, UserSubject(userID), map[string]any{
		"notes": res.Notes, "comments": res.Comments, "reactions": res.Reactions,
	}); err != nil {
		return res, err
	}
	return res, wrapf(tx.Commit(ctx), "обезличивание %d", userID)
}

// ---------------------------------------------------------------- выгрузка

// ExportUser выгружает всё, что площадка знает о человеке, потоком JSON.
//
// Потоком, а не структурой в памяти: у участника с 138 тыс. реплик выгрузка
// весит десятки мегабайт, и собирать её целиком незачем.
//
// Чего в выгрузке НЕТ и не будет: токенов сессий (в базе лежат только их хеши),
// чужих реплик, на которые человек отвечал, и содержимого журнала модерации о
// других людях. Выгрузка — это его данные, а не срез площадки вокруг него.
func (p *Platform) ExportUser(ctx context.Context, userID int64, w io.Writer) error {
	u, err := p.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	write := func(s string) error {
		_, err := io.WriteString(w, s)
		return err
	}
	if err := write("{\n\"выгружено\": "); err != nil {
		return err
	}
	if err := enc.Encode(time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	if err := write(",\n\"участник\": "); err != nil {
		return err
	}
	if err := enc.Encode(map[string]any{
		"id": u.ID, "ник": u.Nick, "вид": int(u.Kind), "роль": int(u.Role),
		"заведён": u.CreatedAt, "последний_визит": u.LastSeenAt,
		"публикации_скрыты": u.HideAll, "запрет_до": u.BannedUntil,
		"обезличен": u.AnonymizedAt,
	}); err != nil {
		return err
	}
	sections := []struct {
		name string
		sql  string
	}{
		{"согласия", `SELECT to_jsonb(x) FROM (
			SELECT kind AS вид, version AS редакция, granted_at AS дано, revoked_at AS отозвано
			  FROM consents WHERE user_id = $1 ORDER BY granted_at) x`},
		{"подтверждения", `SELECT to_jsonb(x) FROM (
			SELECT kind AS вид, external_id AS внешний_id, method AS способ, verified_at AS подтверждено
			  FROM identities WHERE user_id = $1 ORDER BY verified_at) x`},
		{"сессии", `SELECT to_jsonb(x) FROM (
			SELECT created_at AS заведена, last_seen_at AS последняя_активность,
			       expires_at AS истекает, revoked_at AS отозвана, ua AS браузер
			  FROM web_sessions WHERE user_id = $1 ORDER BY created_at) x`},
		{"заметки", `SELECT to_jsonb(x) FROM (
			SELECT id, anonymous AS анонимно, body AS текст, status AS состояние,
			       published_at AS опубликовано, edited_at AS правлено
			  FROM notes WHERE author_id = $1 ORDER BY id) x`},
		{"комментарии", `SELECT to_jsonb(x) FROM (
			SELECT id, note_id AS заметка, reply_to_id AS кому, body AS текст,
			       status AS состояние, published_at AS опубликовано
			  FROM comments WHERE author_id = $1 ORDER BY id) x`},
		{"реакции", `SELECT to_jsonb(x) FROM (
			SELECT note_id AS заметка, comment_id AS комментарий, code AS знак,
			       created_at AS поставлена
			  FROM reactions WHERE user_id = $1 ORDER BY created_at) x`},
		{"мои_жалобы", `SELECT to_jsonb(x) FROM (
			SELECT subject_kind AS на_что, subject_id AS id, reason AS причина,
			       created_at AS подана, resolved_at AS рассмотрена
			  FROM reports WHERE reporter_id = $1 ORDER BY created_at) x`},
		{"модерация_моих_публикаций", `SELECT to_jsonb(x) FROM (
			SELECT subject_kind AS вид, subject_id AS id, category AS категория,
			       reason AS причина, quote AS цитата, verdict AS мнение_автомата,
			       decision AS решение_человека, checked_at AS проверено,
			       decided_at AS решено, appealed_at AS пересмотр_запрошен
			  FROM moderation_queue WHERE author_id = $1 ORDER BY queued_at) x`},
		// Поводы, которые площадка ему показывала. Это его данные, поэтому в
		// выгрузку они идут; ссылками, а не текстами — чужие реплики остаются
		// чужими и здесь (см. шапку про то, чего в выгрузке нет).
		{"мои_события", `SELECT to_jsonb(x) FROM (
			SELECT e.kind AS вид, n.reason AS повод, e.at AS когда,
			       n.read_at AS прочитано, e.note_id AS заметка, e.comment_id AS комментарий
			  FROM notifications n JOIN events e ON e.id = n.event_id
			 WHERE n.user_id = $1 ORDER BY n.event_id) x`},
	}
	for _, s := range sections {
		if err := write(",\n\"" + s.name + "\": ["); err != nil {
			return err
		}
		if err := p.exportRows(ctx, w, s.sql, userID); err != nil {
			return fmt.Errorf("выгрузка %s пользователя %d: %w", s.name, userID, err)
		}
		if err := write("]"); err != nil {
			return err
		}
	}
	return write("\n}\n")
}

// exportRows выливает результат запроса как элементы JSON-массива. Строки
// собирает сам Postgres (to_jsonb), поэтому в Go нет ни одной структуры на
// раздел — добавить поле в выгрузку это одна строка SQL.
func (p *Platform) exportRows(ctx context.Context, w io.Writer, sql string, userID int64) error {
	rows, err := p.pool.Query(ctx, sql, userID)
	if err != nil {
		return err
	}
	defer rows.Close()
	first := true
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		sep := ",\n  "
		if first {
			sep, first = "\n  ", false
		}
		if _, err := io.WriteString(w, sep); err != nil {
			return err
		}
		if _, err := w.Write(raw); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !first {
		_, err = io.WriteString(w, "\n")
	}
	return err
}

// LogExport отмечает в журнале, что выгрузка сделана. Отдельным вызовом, потому
// что сама выгрузка — это поток наружу: она может оборваться на середине, а
// запись «данные отданы» должна означать, что они действительно отданы.
func (p *Platform) LogExport(ctx context.Context, actor Viewer, userID int64) error {
	return audit(ctx, p.pool, actor.UserID, ActionExport, UserSubject(userID), nil)
}
