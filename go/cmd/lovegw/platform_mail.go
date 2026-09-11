package main

// Выдача переписки по запросу уполномоченного органа (эпик L).
//
// Команда существует не «на всякий случай»: обязанность ХРАНИТЬ сообщения и
// обязанность ВЫДАТЬ их по мотивированному запросу установлены одной статьёй
// (ч. 3 ст. 10.1 149-ФЗ) и возникают в один день. Площадка, умеющая только
// копить, — это площадка, которая в день первого запроса будет писать SQL руками
// и под давлением; поэтому выдача заводится тем же шагом, что и таблицы.
//
// Отвечает на запрос ЧЕЛОВЕК, а не команда: он читает бумагу, решает, законна ли
// она и что именно в ней спрошено, и отвечает своим именем. Здесь — только то,
// чем отвечать.
//
// Кнопки у этого нет и не будет — по тому же доводу, что у `platform export` и
// `platform anonymize`, только сильнее: страница, отдающая чужую переписку
// нажатием, однажды отдаст её не тому.

import (
	"context"
	"fmt"
	"os"
	"time"

	"lovegw/internal/config"
	"lovegw/internal/platform"
)

// mailExportDay — формат границ периода. День, а не секунда: в запросе пишут
// «за период с … по …», и требовать от человека часовой пояс с минутами значит
// напрашиваться на ошибку в том единственном месте, где её цена — чужие письма.
const mailExportDay = "2006-01-02"

// platformMailExport выгружает переписку человека за период.
func platformMailExport(ctx context.Context, cfg *config.Config, userID int64, from, to, out string) error {
	if userID == 0 {
		return fmt.Errorf("platform mail-export -user <id участника> [-from 2026-09-01] [-to 2026-09-30] [-out файл]")
	}
	since, err := parseMailBound(from)
	if err != nil {
		return err
	}
	until, err := parseMailBound(to)
	if err != nil {
		return err
	}
	// Верхняя граница — КОНЕЦ названного дня, а не его полночь: «по 30 сентября»
	// в запросе означает «включая тридцатое», и выдать день, обрезанный нулём
	// часов, значит ответить неполно, не заметив этого.
	if !until.IsZero() {
		until = until.Add(24*time.Hour - time.Nanosecond)
	}

	p, err := platform.Open(ctx, cfg.Platform.DSN)
	if err != nil {
		return err
	}
	defer p.Close()

	w := os.Stdout
	if out != "" {
		f, err := os.Create(out)
		if err != nil {
			return fmt.Errorf("файл выдачи: %w", err)
		}
		defer f.Close() //nolint:errcheck // ошибку закрытия видно по неполному файлу
		w = f
	}
	if err := p.ExportMail(ctx, userID, since, until, w); err != nil {
		return err
	}
	// Журнал — ПОСЛЕ выдачи и обязательно: поток мог оборваться, а запись
	// «письма отданы» должна означать, что они действительно отданы.
	actor, err := adminViewer(ctx, p)
	if err != nil {
		return err
	}
	if err := p.LogMailExport(ctx, actor, userID, since, until); err != nil {
		return err
	}
	if out != "" {
		fmt.Fprintf(os.Stderr, "переписка участника %d выдана в %s\n", userID, out)
	}
	return nil
}

// parseMailBound разбирает границу периода. Пустая строка — «без границы»:
// нижнюю и так держат сроки хранения, а верхней у свежего запроса обычно нет.
func parseMailBound(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.ParseInLocation(mailExportDay, s, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("граница периода %q: ожидается ГГГГ-ММ-ДД", s)
	}
	return t, nil
}
