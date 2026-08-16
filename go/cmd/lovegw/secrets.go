package main

// Ключ шифрования сессионных кук и обслуживающая его команда `lovegw secrets`.
//
// Шифрование опциональное: ключа нет — всё работает как раньше, куки лежат
// открыто. Включают его один раз командой `secrets encrypt`, после чего ключ
// обязателен: без него сессии не прочитать. Именно поэтому openStore проверяет
// это на КАЖДОМ открытии базы и падает громко — молча продолжив, демон выдал бы
// всем пользователям «сессия истекла, нужен /login», а на самом деле не подан
// ключ.

import (
	"context"
	"flag"
	"fmt"
	"os"

	"lovegw/internal/acct"
	"lovegw/internal/config"
	"lovegw/internal/secret"
	"lovegw/internal/store"
)

// secretKey достаёт ключ шифрования из конфига/окружения.
func secretKey(cfg *config.Config) (secret.Key, error) {
	return secret.Load(cfg.SecretKey, cfg.SecretKeyFile)
}

// openStore открывает боевую БД с ключом шифрования — единая точка для всех
// команд (talks подменяет cfg.DBPath на копию до вызова). Строки, которые не
// открываются текущим ключом, останавливают запуск.
func openStore(ctx context.Context, cfg *config.Config) (*store.Store, error) {
	key, err := secretKey(cfg)
	if err != nil {
		return nil, err
	}
	path := cfg.DBPath
	st, err := store.Open(ctx, path, store.WithSecret(key))
	if err != nil {
		return nil, err
	}
	stats, err := st.SessionSecretStats(ctx)
	if err != nil {
		st.Close()
		return nil, err
	}
	if stats.Unreadable > 0 {
		st.Close()
		return nil, fmt.Errorf(
			"в %s есть зашифрованные сессии (%d), которые не открываются текущим ключом: "+
				"проверьте LOVEGW_SECRET_KEY / secret_key_file (сводка — lovegw secrets status)",
			path, stats.Unreadable)
	}
	return st, nil
}

// openAccounts открывает базу сервисных аккаунтов сайта. Как и у боевой БД,
// строка, не открывающаяся текущим ключом, останавливает запуск: иначе неверный
// ключ всплыл бы посреди работы, на первом же account say.
func openAccounts(ctx context.Context, cfg *config.Config, path string) (*acct.Store, error) {
	key, err := secretKey(cfg)
	if err != nil {
		return nil, err
	}
	if path == "" {
		path = cfg.AccountsDB()
	}
	db, err := acct.Open(ctx, path, acct.WithSecret(key))
	if err != nil {
		return nil, err
	}
	stats, err := db.SecretStatsOf(ctx)
	if err != nil {
		db.Close()
		return nil, err
	}
	if stats.Unreadable > 0 {
		db.Close()
		return nil, fmt.Errorf(
			"в %s есть зашифрованные аккаунты (%d), которые не открываются текущим ключом: "+
				"проверьте LOVEGW_SECRET_KEY / secret_key_file (сводка — lovegw secrets status)",
			path, stats.Unreadable)
	}
	return db, nil
}

const accountsFlagUsage = "путь к базе сервисных аккаунтов (пусто — accounts.db рядом с боевой БД)"

func cmdSecrets(ctx context.Context, args []string) error {
	sub, rest := splitSubcommand(args, map[string]bool{
		"keygen": true, "status": true, "encrypt": true,
	})
	switch sub {
	case "keygen":
		return secretsKeygen(rest)
	case "status":
		return secretsStatus(ctx, rest)
	case "encrypt":
		return secretsEncrypt(ctx, rest)
	default:
		usage()
		return fmt.Errorf("secrets: нужна подкоманда (keygen|status|encrypt)")
	}
}

// secretsKeygen печатает новый ключ. Это единственный секрет, который проект
// выводит на экран: он ещё нигде не хранится, иначе его не передать оператору.
func secretsKeygen(args []string) error {
	fs := flag.NewFlagSet("secrets keygen", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	key, err := secret.Generate()
	if err != nil {
		return err
	}
	fmt.Println(key)
	fmt.Fprintln(os.Stderr, `
Положите ключ в LOVEGW_SECRET_KEY (или в файл из secret_key_file) и храните
ОТДЕЛЬНО от бэкапов базы — в этом весь смысл: в копии БД ключа нет.
Потеря ключа = потеря всех сессий: пользователям придётся заново /login,
сервисным аккаунтам — account login.
Дальше: lovegw secrets encrypt (зашифровать то, что уже лежит открыто).`)
	return nil
}

func secretsStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("secrets status", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, configFlagUsage)
	accountsPath := fs.String("accounts", "", accountsFlagUsage)
	if err := fs.Parse(reorderArgs(args, fs)); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	key, err := secretKey(cfg)
	if err != nil {
		return err
	}
	if key.Enabled() {
		fmt.Println("ключ шифрования: задан")
	} else {
		// Без ключа шифротекст в базе неотличим от «ещё не шифровали» только на
		// глаз: цифры ниже покажут разницу, поэтому здесь без выводов.
		fmt.Println("ключ шифрования: НЕ задан")
	}

	// Базу открываем напрямую, мимо openStore: сводка нужна как раз тогда,
	// когда ключ не подходит, а openStore в этом случае откажет.
	st, err := store.Open(ctx, cfg.DBPath, store.WithSecret(key))
	if err != nil {
		return err
	}
	defer st.Close()
	sessions, err := st.SessionSecretStats(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("\nсессии пользователей (%s):\n", cfg.DBPath)
	printSecretStats(sessions.Total, sessions.Plain, sessions.Encrypted, sessions.Unreadable)

	accounts, err := openAccounts(ctx, cfg, *accountsPath)
	if err != nil {
		return err
	}
	defer accounts.Close()
	as, err := accounts.SecretStatsOf(ctx)
	if err != nil {
		return err
	}
	path := *accountsPath
	if path == "" {
		path = cfg.AccountsDB()
	}
	fmt.Printf("\nсервисные аккаунты (%s):\n", path)
	printSecretStats(as.Total, as.Plain, as.Encrypted, as.Unreadable)

	if sessions.Unreadable > 0 || as.Unreadable > 0 {
		fmt.Println("\nЕсть записи, которые не открываются текущим ключом. Если ключ сменили —")
		fmt.Println("перешейте базы: lovegw secrets encrypt -old-key-env LOVEGW_SECRET_KEY_OLD")
	}
	if sessions.Plain > 0 || as.Plain > 0 {
		fmt.Println("\nЧасть записей лежит открыто: lovegw secrets encrypt")
	}
	return nil
}

func printSecretStats(total, plain, encrypted, unreadable int) {
	fmt.Printf("  всего записей:   %d\n", total)
	fmt.Printf("  открыто:         %d\n", plain)
	fmt.Printf("  зашифровано:     %d\n", encrypted)
	if unreadable > 0 {
		fmt.Printf("  НЕ открывается:  %d\n", unreadable)
	}
}

// secretsEncrypt перешивает обе базы под текущий ключ. Идемпотентно: записи,
// уже лежащие под ним, не трогаются, поэтому повтор после сбоя безопасен.
func secretsEncrypt(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("secrets encrypt", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, configFlagUsage)
	accountsPath := fs.String("accounts", "", accountsFlagUsage)
	oldKeyEnv := fs.String("old-key-env", "", "переменная со СТАРЫМ ключом — для ротации")
	if err := fs.Parse(reorderArgs(args, fs)); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	key, err := secretKey(cfg)
	if err != nil {
		return err
	}
	if !key.Enabled() {
		return fmt.Errorf("не задан ключ шифрования: LOVEGW_SECRET_KEY или secret_key_file (новый — lovegw secrets keygen)")
	}
	var old secret.Key
	if *oldKeyEnv != "" {
		if old, err = secret.Parse(os.Getenv(*oldKeyEnv)); err != nil {
			return fmt.Errorf("старый ключ из %s: %w", *oldKeyEnv, err)
		}
		if !old.Enabled() {
			return fmt.Errorf("переменная %s пуста", *oldKeyEnv)
		}
	}

	// Напрямую, мимо openStore: при ротации записи как раз НЕ открываются
	// текущим ключом — это нормальное состояние, ради которого команду и зовут.
	st, err := store.Open(ctx, cfg.DBPath, store.WithSecret(key))
	if err != nil {
		return err
	}
	defer st.Close()
	n, err := st.ReencryptSessions(ctx, old)
	if err != nil {
		return err
	}
	fmt.Printf("сессии пользователей: перешито %d\n", n)

	accounts, err := openAccounts(ctx, cfg, *accountsPath)
	if err != nil {
		return err
	}
	defer accounts.Close()
	n, err = accounts.Reencrypt(ctx, old)
	if err != nil {
		return err
	}
	fmt.Printf("сервисные аккаунты:   перешито %d\n", n)
	fmt.Fprintln(os.Stderr, "\nТеперь ключ обязателен: без него сессии не прочитать.")
	return nil
}
