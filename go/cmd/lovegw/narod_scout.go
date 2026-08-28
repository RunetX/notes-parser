package main

// lovegw narod scout — подбор доноров под состав.
//
// Первый шаг конвейера производства жителя, и он же ответ на замер 28.08.2026:
// разговор между жителями заводится от их ЧИСЛА, а не от настройки кубика.
// Добирать состав «покрупнее корпусом» бесполезно — нужен СОСЕД по тредам, то
// есть тот, кто на самом деле сидел рядом и отвечал тем же людям.
//
// Выбирает всё равно владелец: команда только показывает числа, по которым
// выбор можно сделать. Ни в сеть, ни в Postgres не ходит — читается archive.db.

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"lovegw/internal/archive"
)

// scoutOpts — параметры подбора.
type scoutOpts struct {
	dbPath      string
	with        string // состав, вокруг которого ищем: u<id> через запятую
	threads     int    // сколько последних тредов состава просмотреть
	minComments int    // корпус кандидата, ниже которого слепок не снять
	limit       int
}

// scoutMinComments — минимальный корпус донора.
//
// Полторы тысячи, потому что слепок меряет не только манеру: полоса голоса
// набирается ПАЧКАМИ по 600 знаков (двести отложенных реплик), кривая отклика
// считается по последним тремстам тредам, а характерные ошибки — сравнением с
// нормой корпуса. У человека с полутора сотнями реплик всё это выйдет числами,
// которые нельзя отличить от шума.
const scoutMinComments = 1500

func narodScout(ctx context.Context, o scoutOpts) error {
	ids, err := parseActorIDs(o.with)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return fmt.Errorf("narod scout: нужен состав, вокруг которого искать: -with u123,u456")
	}
	ar, err := archive.Open(ctx, o.dbPath)
	if err != nil {
		return err
	}
	defer ar.Close()

	list, err := ar.MineCoParticipants(ctx, ids, o.threads, o.minComments, o.limit)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		return fmt.Errorf("narod scout: соседей с корпусом от %d реплик не нашлось — "+
			"опустите -min-comments", o.minComments)
	}

	fmt.Fprintf(os.Stderr, "соседей по последним %d тредам состава: %d\n\n", o.threads, len(list))
	fmt.Printf("| анкета | ник | пол | корпус | тредов рядом | сказал | ОТВЕТОВ друг другу | на сайте |\n")
	fmt.Printf("|---|---|---|---:|---:|---:|---:|---|\n")
	for _, p := range list {
		fmt.Printf("| u%d | %s | %s | %d | %d | %d | %d | %s — %s |\n",
			p.UserID, p.Nick, orDash(genderRu(p.Gender)), p.Comments, p.Threads, p.Said,
			p.Replies, orDash(dateOnly(p.First)), orDash(dateOnly(p.Last)))
	}
	fmt.Fprintf(os.Stderr, "\nГлавный столбец — ОТВЕТЫ друг другу: сосед по треду бывает случайным, "+
		"собеседник нет. Эпоху смотреть обязательно: замолчавший до прихода состава "+
		"не встретится с ним ни в одном треде.\nСлепок снимается `narod card u<id>`.\n")
	return nil
}

// parseActorIDs — список анкет u<id> через запятую.
func parseActorIDs(s string) ([]int64, error) {
	var out []int64
	for _, t := range splitTokens(s) {
		id, err := strconv.ParseInt(strings.TrimPrefix(t, "u"), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("анкеты задаются как u<id>, а не %q", t)
		}
		out = append(out, id)
	}
	return out, nil
}
