package main

// narod enroll и narod seed — последние два шага конвейера производства
// персонажа и первый шаг его жизни.
//
// ENROLL заводит жителю АНКЕТУ на площадке и строку в мире. Разделение
// намеренное: карточка — документ, который правят руками и держат вне git
// (она выведена из писем живых людей), а номер анкеты выдаёт Postgres, и
// класть его обратно в файл значило бы, что карточку нельзя перенести на
// другой стенд. Связка живёт в МИРЕ, в таблице actors, вместе с отношениями.
//
// SEED поднимает заметку-ПЕСОЧНИЦУ — сцену, на которой жители играют. Двумя
// путями, и оба нужны: своим текстом (владелец пишет, что хочет обсудить) и
// «поднять архивную» — та же заметка, что когда-то шла на НГС, с указанием на
// оригинал. Второе есть прямой перенос калибровки в бой: реплей десять раз
// прогонял эти же заметки, и первые живые сцены честнее брать оттуда же —
// сравнивать будет с чем.
//
// Обе команды идут ЧЕРЕЗ ЯДРО площадки, а не INSERT'ом: право писать, потолки,
// очередь модерации и событие шины у заметки-песочницы обязаны быть теми же,
// что у всякой другой.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"lovegw/internal/config"
	"lovegw/internal/narod"
	"lovegw/internal/platform"
)

// narodEnroll заводит жителя на площадке.
//
// Идемпотентна: карточку правят и `enroll` зовут второй раз, а заводить от
// этого второго жителя нельзя — у первого уже есть память и отношения. Признаком
// «уже заведён» служит строка в мире, а не поиск по нику: тёзки на площадке
// законны, и опознавать людей по имени она не умеет нигде.
func narodEnroll(ctx context.Context, cfg *config.Config, cardsDir, worldPath string, slugs []string) error {
	if len(slugs) == 0 {
		return fmt.Errorf("narod enroll: назовите жителя (слуг карточки)")
	}
	p, err := platform.Open(ctx, cfg.Platform.DSN)
	if err != nil {
		return err
	}
	defer p.Close()

	world, err := narod.OpenWorld(ctx, worldPath)
	if err != nil {
		return err
	}
	defer world.Close()

	now := time.Now()
	for _, slug := range slugs {
		card, err := narod.LoadCard(cardPath(cardsDir, slug))
		if err != nil {
			return err
		}
		// Слепок на площадку не выходит НИКОГДА: это была бы имперсонация живого
		// человека под его же ником. Правило записано трижды — здесь, в сборке
		// службы и в самом ядре, — и дублирование намеренное: цена ошибки не
		// поломка, а публикация.
		if card.Kind != narod.KindComposite {
			return fmt.Errorf("%s — %s: наружу выходят только композиты", card.ID, card.Kind)
		}
		if err := card.Validate(); err != nil {
			return err
		}
		if a, err := worldActor(ctx, world, card.ID); err != nil {
			return err
		} else if a.PlatformUserID != 0 {
			fmt.Printf("%s уже заведён: анкета %d\n", card.ID, a.PlatformUserID)
			continue
		}
		id, err := p.CreatePersonaUser(ctx, card.Persona.Nick)
		if err != nil {
			return fmt.Errorf("житель %s: %w", card.ID, err)
		}
		// Пол ставится сразу и из карточки: без него страница нарисует
		// нейтральный силуэт, а житель у нас всегда мужчина или женщина — это
		// половина того, из чего сложен его голос, и рычаг разнополого разговора
		// в кубике опирается ровно на него.
		if g := genderOf(card.Persona.Gender); g != platform.GenderUnknown {
			if _, err := p.SetGenders(ctx, map[int64]platform.Gender{id: g}); err != nil {
				return fmt.Errorf("пол жителя %s: %w", card.ID, err)
			}
		}
		if err := world.UpsertActor(ctx, narod.Actor{
			ID: card.ID, Kind: narod.ActorPersona, PlatformUserID: id,
			Nick: card.Persona.Nick, CardPath: cardPath(cardsDir, card.ID),
		}, now); err != nil {
			return err
		}
		fmt.Printf("%s заведён: %s, анкета %d — %s/u/%d\n",
			card.ID, card.Persona.Nick, id, cfg.Platform.BaseURL, id)
	}
	return nil
}

// worldActor — строка жителя в мире; отсутствие не ошибка.
func worldActor(ctx context.Context, w *narod.World, id string) (narod.Actor, error) {
	all, err := w.Actors(ctx)
	if err != nil {
		return narod.Actor{}, err
	}
	for _, a := range all {
		if a.ID == id {
			return a, nil
		}
	}
	return narod.Actor{}, nil
}

func genderOf(s string) platform.Gender {
	switch s {
	case "male":
		return platform.GenderMale
	case "female":
		return platform.GenderFemale
	}
	return platform.GenderUnknown
}

// narodSeed заводит заметку-песочницу.
//
// Автор её — АДМИНИСТРАТОР площадки, а не житель, и это не мелочь: заметку
// пишет садовник, а жители на неё приходят. Житель, заведший собственную
// заметку, был бы уже не участником мира, а его источником — и первое же
// «обсудите вот это» от машины сломало бы замысел.
func narodSeed(ctx context.Context, cfg *config.Config, bodyPath string, from int64) error {
	p, err := platform.Open(ctx, cfg.Platform.DSN)
	if err != nil {
		return err
	}
	defer p.Close()

	actor, err := adminViewer(ctx, p)
	if err != nil {
		return err
	}
	var body string
	switch {
	case bodyPath != "":
		if body, err = readTextArg(bodyPath); err != nil {
			return err
		}
	case from != 0:
		if body, err = archiveNoteBody(ctx, p, from); err != nil {
			return err
		}
	default:
		return fmt.Errorf("narod seed: назовите текст (-body файл|-) либо архивную заметку (-from <id>)")
	}
	id, err := p.CreateNote(ctx, platform.NewNote{
		AuthorID: actor.UserID, Body: body, Stage: true,
	})
	if err != nil {
		return err
	}
	fmt.Printf("песочница %d заведена: %s/n/%d\n", id, cfg.Platform.BaseURL, id)
	fmt.Println("жители придут сами, когда служба увидит её очередным смотром")
	return nil
}

// archiveNoteBody — текст архивной заметки плюс строка-указание на оригинал.
//
// Указание обязательно и стоит В ТЕЛЕ, а не пометкой: заметка-песочница это НАША
// запись, а текст в ней чужой, и читателю, наткнувшемуся на знакомые слова,
// нужно знать, откуда они. Ссылка при этом ведёт НА ПЛОЩАДКУ (архив раскатан, и
// номера совпадают с сайтом), а не на НГС: ссылок туда проект не ставит нигде.
func archiveNoteBody(ctx context.Context, p *platform.Platform, id int64) (string, error) {
	n, err := p.NoteRow(ctx, id)
	if err != nil {
		return "", fmt.Errorf("архивная заметка %d: %w", id, err)
	}
	body := strings.TrimSpace(n.Body)
	if body == "" {
		return "", fmt.Errorf("архивная заметка %d пуста", id)
	}
	return body + "\n\n(разговор поднят заново, оригинал — /n/" +
		fmt.Sprint(id) + ")", nil
}

// narodStage переводит уже стоящую в ленте заметку в песочницу и обратно.
//
// Нужна потому, что материал для песочницы чаще всего УЖЕ написан: своя
// заметка, которую никто не подхватил, — готовая сцена, и копия её текста
// новой записью означала бы тот же текст дважды в одной ленте.
//
// Правило «только пока никто не говорил» держит ядро, а не команда: причина
// там же, где и проверка (см. platform.SetNoteStageAsAdmin).
func narodStage(ctx context.Context, cfg *config.Config, noteID int64, off bool, reason string) error {
	if noteID == 0 {
		return fmt.Errorf("narod stage: назовите заметку (-note <id>)")
	}
	p, err := platform.Open(ctx, cfg.Platform.DSN)
	if err != nil {
		return err
	}
	defer p.Close()

	actor, err := adminViewer(ctx, p)
	if err != nil {
		return err
	}
	if err := p.SetNoteStageAsAdmin(ctx, actor, noteID, !off, reason); err != nil {
		return err
	}
	if off {
		fmt.Printf("заметка %d возвращена людям: %s/n/%d\n", noteID, cfg.Platform.BaseURL, noteID)
		return nil
	}
	fmt.Printf("заметка %d стала песочницей: %s/n/%d\n", noteID, cfg.Platform.BaseURL, noteID)
	fmt.Println("жители придут сами, когда служба увидит её очередным смотром")
	return nil
}
