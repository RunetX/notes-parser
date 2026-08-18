package main

// Разовое сообщение владельцу в ЛС — вход в уже существующие алерты для того,
// что живёт ВНЕ демона.
//
// Заведено ради бэкапа (Ш8): он работает кроном на хосте, и его отказ иначе
// виден только тому, кто пойдёт читать лог, — то есть никому. Демон про эти
// файлы знать не может, каталог бэкапов в контейнеры не смонтирован и монтировать
// его туда незачем: базе данных нечего делать в каталоге своих же копий. А
// заводить вторую систему уведомлений ради одной строки тем более незачем —
// личка владельцу у проекта есть, не хватало только двери в неё снаружи.
//
//	docker exec lovegw /lovegw alert -config /config.json "бэкап не собрался"
//	tail -5 backup.log | docker exec -i lovegw /lovegw alert -config /config.json
//
// Текст берётся из аргументов, а если их нет — со stdin: так удобно отдать хвост
// лога, и длинная строка не светится в списке процессов.

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"lovegw/internal/config"
	"lovegw/internal/maxx"
	"lovegw/internal/tgx"
)

// alertLimit — потолок текста. Больше в сообщение мессенджера всё равно не
// влезет, а обрезать хвост лога лучше нам, чем получить отказ отправки.
const alertLimit = 3000

func cmdAlert(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("alert", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, configFlagUsage)
	if err := fs.Parse(reorderArgs(args, fs)); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	text := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if text == "" {
		raw, err := io.ReadAll(io.LimitReader(os.Stdin, alertLimit))
		if err != nil {
			return fmt.Errorf("чтение текста со stdin: %w", err)
		}
		text = strings.TrimSpace(string(raw))
	}
	if text == "" {
		return fmt.Errorf("нечего отправлять: текст пуст")
	}
	if len([]rune(text)) > alertLimit {
		text = string([]rune(text)[:alertLimit]) + "…"
	}
	text = "⚠️ lovegw: " + text

	var sent []string
	var failed []error

	// Порядок ботов тот же, что у алертов демона: сперва бот переписки, потом
	// командный, и лишь в крайнем случае постер — писать в ЛС первым он умеет
	// не всегда. Чужому боту человек мог и не открывать личку.
	if tg := cfg.Messengers.Telegram; tg.Enabled && tg.AdminUserID != 0 {
		if token := firstToken(tg.TalksToken, tg.DMToken, tg.Token); token == "" {
			failed = append(failed, fmt.Errorf("telegram: нет ни одного токена"))
		} else {
			hc, err := tgx.ProxyClient(cfg.TelegramProxy)
			if err != nil {
				return fmt.Errorf("telegram: прокси: %w", err)
			}
			bot, err := tgx.NewMirror(tgx.Params{Token: token, HTTPClient: hc}, nil, nil)
			if err != nil {
				failed = append(failed, fmt.Errorf("telegram: %w", err))
			} else if err := bot.SendText(ctx, tg.AdminUserID, text); err != nil {
				failed = append(failed, fmt.Errorf("telegram: %w", err))
			} else {
				sent = append(sent, "telegram")
			}
		}
	}

	if mx := cfg.Messengers.Max; mx.Enabled && mx.AdminUserID != 0 {
		if token := firstToken(mx.TalksToken, mx.DMToken, mx.Token); token == "" {
			failed = append(failed, fmt.Errorf("max: нет ни одного токена"))
		} else if bot, err := maxx.NewMirror(maxx.Params{
			Token:      token,
			HTTPClient: maxx.MintsifraClient(),
		}, nil); err != nil {
			failed = append(failed, fmt.Errorf("max: %w", err))
		} else if err := bot.SendText(ctx, mx.AdminUserID, text); err != nil {
			failed = append(failed, fmt.Errorf("max: %w", err))
		} else {
			sent = append(sent, "max")
		}
	}

	// Достаточно одного дошедшего: алерт — это «человек узнал», а не «дошло
	// везде». Но молчать про неудавшиеся нельзя, иначе однажды окажется, что
	// работал ровно один канал и он же отвалился.
	for _, err := range failed {
		fmt.Fprintln(os.Stderr, "не отправлено:", err)
	}
	if len(sent) == 0 {
		return fmt.Errorf("алерт не отправлен никуда (проверьте admin_user_id и токены)")
	}
	fmt.Println("отправлено:", strings.Join(sent, ", "))
	return nil
}

func firstToken(tokens ...string) string {
	for _, t := range tokens {
		if t != "" {
			return t
		}
	}
	return ""
}
