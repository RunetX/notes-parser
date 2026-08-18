package main

// Модерация из командной строки (Ш7).
//
// Три команды здесь, и они не дублируют веб-морду, а закрывают ровно то, чего в
// ней нет и не будет.
//
//   - `moderation` — сводка: наполнение очереди и последние действия. Нужна
//     затем, что «очередь растёт быстрее, чем её читают» — это единственный
//     признак, по которому владелец узнаёт, что порог автомата пора поднимать, а
//     заходить ради него на страницу каждый день он не станет.
//   - `anonymize` и `export` — права субъекта. Кнопкой они не сделаны намеренно:
//     обе операции проходят по ВСЕМ публикациям человека, а такой проход стоит
//     десятки секунд (замер 18.08.2026: 53 с у автора со 138 тыс. реплик) и в
//     срок веб-запроса не влезает вовсе. Обезличивание вдобавок необратимо, и
//     подтверждение ему нужно не галочкой, а перепиской. Закон это допускает
//     прямо: на требование субъекта у оператора тридцать дней, а самообслуживанием
//     обязан работать отзыв согласия — он им и работает, кнопкой на /me.
//   - `ban` / `unban` — те же действия, что на карточке участника, но доступные,
//     когда модератора ещё не назначили: первого администратора тоже назначают
//     отсюда.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"lovegw/internal/config"
	"lovegw/internal/platform"
)

// oneUserID разбирает единственный аргумент-номер участника.
func oneUserID(tail []string, usage string) (int64, error) {
	if len(tail) != 1 {
		return 0, errors.New(usage)
	}
	id, err := strconv.ParseInt(tail[0], 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("id участника %q: ожидалось число", tail[0])
	}
	return id, nil
}

// platformModeration печатает состояние очереди и последние решения.
func platformModeration(ctx context.Context, cfg *config.Config, limit int) error {
	p, err := platform.Open(ctx, cfg.Platform.DSN)
	if err != nil {
		return err
	}
	defer p.Close()

	stats, err := p.ModerationStats(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "ждут решения человека\t%d\n", stats.Review)
	fmt.Fprintf(w, "скрыто автоматом (не пересмотрено)\t%d\n", stats.AutoHidden)
	fmt.Fprintf(w, "просьб о пересмотре\t%d\n", stats.Appeals)
	fmt.Fprintf(w, "открытых жалоб\t%d\n", stats.Reports)
	fmt.Fprintf(w, "не проверено автоматом\t%d\n", stats.Unchecked)
	fmt.Fprintf(w, "скрыто автоматом за сутки\t%d\n", stats.HiddenDay)
	w.Flush() //nolint:errcheck // печать в stdout

	// Цель владельца — единицы строк в сутки. Если очередь длиннее, поднимать
	// надо порог автомата, а не усердие: интерфейс, ради которого приходится
	// «заходить и разбираться», ничем не лучше его отсутствия.
	if stats.Review > 20 {
		fmt.Printf("\nочередь длиннее двух десятков — порог автомата стоит поднять\n")
	}

	queue, err := p.ReviewQueue(ctx, limit)
	if err != nil {
		return err
	}
	if len(queue) > 0 {
		fmt.Printf("\nждут решения:\n")
		q := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, it := range queue {
			mark := ""
			if it.Appealed() {
				mark = " ПЕРЕСМОТР"
			}
			fmt.Fprintf(q, "%s %d\t%s\t%s\t%s%s\n",
				it.Subject.Kind, it.Subject.ID, it.AuthorNick,
				it.CategoryTitle(), oneLine(it.Body, 60), mark)
		}
		q.Flush() //nolint:errcheck // печать в stdout
	}

	log, err := p.AuditTail(ctx, 20)
	if err != nil {
		return err
	}
	if len(log) > 0 {
		fmt.Printf("\nпоследние действия:\n")
		l := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, e := range log {
			who := e.Nick
			if who == "" {
				who = "автомат"
			}
			fmt.Fprintf(l, "%s\t%s\t%s\t%s %d\n",
				e.At.In(time.Local).Format("02.01 15:04"), e.Action, who, e.Subject.Kind, e.Subject.ID)
		}
		l.Flush() //nolint:errcheck // печать в stdout
	}
	return nil
}

// oneLine сжимает текст в одну строку заданной длины: очередь читают в
// терминале, и трёхабзацная заметка ломает выравнивание таблицы.
func oneLine(s string, max int) string {
	r := []rune(s)
	for i, c := range r {
		if c == '\n' || c == '\r' || c == '\t' {
			r[i] = ' '
		}
	}
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return string(r)
}

// platformBan запрещает и разрешает публикации.
func platformBan(ctx context.Context, cfg *config.Config, id int64, ban bool, days int, reason string) error {
	p, err := platform.Open(ctx, cfg.Platform.DSN)
	if err != nil {
		return err
	}
	defer p.Close()

	// Актор — админ площадки, а не нулевой: бан это решение о ЧЕЛОВЕКЕ, и в
	// журнале у него обязан быть автор. Прав команде тоже неоткуда взять иначе:
	// ядро проверяет роль, а не то, откуда пришёл вызов.
	actor, err := adminViewer(ctx, p)
	if err != nil {
		return err
	}
	if ban {
		until := time.Now().AddDate(0, 0, days)
		if err := p.BanUser(ctx, actor, id, until, reason); err != nil {
			return err
		}
		fmt.Printf("публикации запрещены до %s\n", until.Format("02.01.2006 15:04"))
		return nil
	}
	if err := p.UnbanUser(ctx, actor, id, reason); err != nil {
		if errors.Is(err, platform.ErrNothingToDo) {
			fmt.Println("запрета и не было")
			return nil
		}
		return err
	}
	fmt.Println("запрет снят")
	return nil
}

// platformAnonymize исполняет требование субъекта об обезличивании.
func platformAnonymize(ctx context.Context, cfg *config.Config, id int64, yes bool) error {
	p, err := platform.Open(ctx, cfg.Platform.DSN)
	if err != nil {
		return err
	}
	defer p.Close()

	u, err := p.UserByID(ctx, id)
	if err != nil {
		return err
	}
	if !yes {
		// Подтверждение флагом, а не вопросом в терминал: команду запускают и по
		// ssh из скрипта, а необратимое действие не должно зависеть от того,
		// подключён ли stdin.
		fmt.Printf("обезличить %q (id %d)?\n", u.Nick, u.ID)
		fmt.Println("тексты останутся на месте — удалить их, не разрушив чужие разговоры, нельзя;")
		fmt.Println("уйдут имя, фото, пол и связь с анкетой НГС, а публикации переедут на")
		fmt.Println("безымянную запись, соответствие с которой нигде не сохраняется.")
		fmt.Println("операция НЕОБРАТИМА. повторите с ключом -yes")
		return nil
	}
	actor, err := adminViewer(ctx, p)
	if err != nil {
		return err
	}
	res, err := p.AnonymizeUser(ctx, actor, id)
	if err != nil {
		return err
	}
	fmt.Printf("обезличено: заметок %d, комментариев %d, реакций %d\n",
		res.Notes, res.Comments, res.Reactions)
	return nil
}

// platformExport выгружает всё, что площадка знает о человеке.
func platformExport(ctx context.Context, cfg *config.Config, id int64, out string) error {
	p, err := platform.Open(ctx, cfg.Platform.DSN)
	if err != nil {
		return err
	}
	defer p.Close()

	w := os.Stdout
	if out != "" {
		f, err := os.Create(out)
		if err != nil {
			return fmt.Errorf("файл выгрузки: %w", err)
		}
		defer f.Close() //nolint:errcheck // ошибку закрытия видно по неполному файлу
		w = f
	}
	if err := p.ExportUser(ctx, id, w); err != nil {
		return err
	}
	// Журнал пишется ПОСЛЕ выгрузки: она идёт потоком и может оборваться, а
	// запись «данные отданы» должна означать, что они действительно отданы.
	actor, err := adminViewer(ctx, p)
	if err != nil {
		return err
	}
	if err := p.LogExport(ctx, actor, id); err != nil {
		return err
	}
	if out != "" {
		fmt.Fprintf(os.Stderr, "выгружено в %s\n", out)
	}
	return nil
}

// adminViewer — от чьего имени действует команда. Берётся ЛЮБОЙ администратор
// площадки, а не нулевой актор, по двум причинам: ядро проверяет права (роль, а
// не происхождение вызова), и решение о человеке обязано иметь автора в журнале.
//
// Первого администратора назначают `platform role <id> admin` — там актор
// нулевой намеренно, потому что назначать его больше некому.
func adminViewer(ctx context.Context, p *platform.Platform) (platform.Viewer, error) {
	id, err := p.AnyAdmin(ctx)
	if err != nil {
		return platform.Viewer{}, fmt.Errorf(
			"на площадке нет администратора: назначьте его `platform role <id> admin`: %w", err)
	}
	return platform.Viewer{UserID: id, Role: platform.RoleAdmin}, nil
}
