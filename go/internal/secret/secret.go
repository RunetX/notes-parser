// Пакет secret шифрует значения, которые лежат в SQLite: сессионные куки в
// боевой БД (таблица sessions) и в accounts.db.
//
// Что это защищает: КОПИИ базы — бэкапы `sqlite3 .backup`, БД, уехавшую на
// рабочую машину, случайно приложенный к чему-нибудь файл. Ключ хранится вне
// базы (env или отдельный файл), поэтому в копии его нет. От компрометации
// самого хоста шифрование не спасает: там доступно всё, что доступно демону.
//
// Алгоритм — AES-256-GCM из стандартной библиотеки: новых зависимостей проекту
// не нужно, а образ distroless собирается из того, что уже есть.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Prefix помечает зашифрованное значение. Версия в префиксе нужна, чтобы
// сменить формат, не гадая по длине строки: старые записи узнаются точно.
const Prefix = "enc:v1:"

// KeySize — 32 байта, то есть AES-256.
const KeySize = 32

// ErrNoKey — в базе шифротекст, а ключа нет. Отдельная ошибка: вызывающий
// отличает «нечем расшифровать» от «сессии нет» и не выдаёт человеку
// «перелогиньтесь» там, где на самом деле не подан ключ.
var ErrNoKey = errors.New("значение зашифровано, а ключ не задан (LOVEGW_SECRET_KEY)")

// Key — ключ шифрования. Нулевое значение рабочее и означает «шифрование
// выключено»: Seal отдаёт открытый текст, Open читает открытые записи.
type Key struct {
	aead cipher.AEAD
}

// Enabled — задан ли ключ.
func (k Key) Enabled() bool { return k.aead != nil }

// Parse разбирает ключ из base64 (стандартный алфавит). Пустая строка — ключа
// нет, это не ошибка.
func Parse(b64 string) (Key, error) {
	b64 = strings.TrimSpace(b64)
	if b64 == "" {
		return Key{}, nil
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return Key{}, fmt.Errorf("ключ шифрования: не разобран base64: %w", err)
	}
	if len(raw) != KeySize {
		return Key{}, fmt.Errorf("ключ шифрования: нужно %d байт, получено %d", KeySize, len(raw))
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return Key{}, fmt.Errorf("ключ шифрования: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return Key{}, fmt.Errorf("ключ шифрования: %w", err)
	}
	return Key{aead: aead}, nil
}

// Load берёт ключ из явного значения (env/конфиг) либо из файла. Заданы оба —
// выигрывает явное значение: env перебивает конфиг всюду в проекте.
func Load(keyB64, keyFile string) (Key, error) {
	if strings.TrimSpace(keyB64) != "" {
		return Parse(keyB64)
	}
	if strings.TrimSpace(keyFile) == "" {
		return Key{}, nil
	}
	data, err := os.ReadFile(keyFile)
	if err != nil {
		return Key{}, fmt.Errorf("файл ключа %s: %w", keyFile, err)
	}
	return Parse(string(data))
}

// Generate возвращает новый случайный ключ в base64 — то, что кладут в
// LOVEGW_SECRET_KEY.
func Generate() (string, error) {
	raw := make([]byte, KeySize)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// Encrypted — зашифровано ли значение.
func Encrypted(s string) bool { return strings.HasPrefix(s, Prefix) }

// Seal шифрует значение. Без ключа возвращает открытый текст как есть, чтобы
// вызывающему не приходилось ветвиться.
//
// aad — контекст записи («sessions:telegram:123456», «accounts:reserve»). Он не
// шифруется, но входит в проверку целостности: шифротекст, переставленный в
// чужую строку или чужую таблицу, не расшифруется.
func (k Key) Seal(aad, plain string) (string, error) {
	if !k.Enabled() {
		return plain, nil
	}
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("шифрование %s: nonce: %w", aad, err)
	}
	sealed := k.aead.Seal(nonce, nonce, []byte(plain), []byte(aad))
	return Prefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Open расшифровывает значение. Строка без префикса отдаётся как есть — так
// читаются записи, сделанные до включения шифрования. Шифротекст без ключа —
// ErrNoKey.
func (k Key) Open(aad, stored string) (string, error) {
	if !Encrypted(stored) {
		return stored, nil
	}
	if !k.Enabled() {
		return "", ErrNoKey
	}
	sealed, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, Prefix))
	if err != nil {
		return "", fmt.Errorf("расшифровка %s: не разобран base64: %w", aad, err)
	}
	ns := k.aead.NonceSize()
	if len(sealed) < ns {
		return "", fmt.Errorf("расшифровка %s: значение короче nonce", aad)
	}
	plain, err := k.aead.Open(nil, sealed[:ns], sealed[ns:], []byte(aad))
	if err != nil {
		// Сюда попадают и порча байтов, и чужой ключ, и перенос шифротекста в
		// другую строку: GCM не различает эти случаи, и различать их нам нечем.
		return "", fmt.Errorf("расшифровка %s: значение не сходится с ключом", aad)
	}
	return string(plain), nil
}

// Reveal достаёт открытое значение для перешивки под другой ключ (включение
// шифрования или ротация). need == false — запись уже лежит под текущим
// ключом, трогать её незачем; на этом держится идемпотентность перешивки.
func Reveal(aad, stored string, cur, old Key) (plain string, need bool, err error) {
	if !Encrypted(stored) {
		return stored, true, nil // открытая запись — зашифровать
	}
	if _, err := cur.Open(aad, stored); err == nil {
		return "", false, nil
	}
	if !old.Enabled() {
		return "", false, fmt.Errorf("не открывается текущим ключом, а старый не задан (-old-key-env)")
	}
	plain, err = old.Open(aad, stored)
	if err != nil {
		return "", false, fmt.Errorf("не открывается ни текущим ключом, ни старым")
	}
	return plain, true, nil
}

// SessionAAD — контекст сессии пользователя мессенджера.
func SessionAAD(messenger string, userID int64) string {
	return fmt.Sprintf("sessions:%s:%d", messenger, userID)
}

// AccountAAD — контекст сервисного аккаунта сайта.
func AccountAAD(name string) string { return "accounts:" + name }
