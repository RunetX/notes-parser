package secret

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKey(t *testing.T) Key {
	t.Helper()
	b64, err := Generate()
	if err != nil {
		t.Fatalf("генерация ключа: %v", err)
	}
	k, err := Parse(b64)
	if err != nil {
		t.Fatalf("разбор ключа: %v", err)
	}
	if !k.Enabled() {
		t.Fatal("сгенерированный ключ считается пустым")
	}
	return k
}

func TestSealOpenRoundTrip(t *testing.T) {
	k := testKey(t)
	const aad = "sessions:telegram:42"
	plain := `[{"name":"ngs_ttq","value":"SESSION"}]`

	sealed, err := k.Seal(aad, plain)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !Encrypted(sealed) {
		t.Fatalf("нет префикса %q: %q", Prefix, sealed)
	}
	if strings.Contains(sealed, "SESSION") {
		t.Fatal("открытое значение видно в шифротексте")
	}
	got, err := k.Open(aad, sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got != plain {
		t.Fatalf("после расшифровки %q, ожидалось %q", got, plain)
	}
}

// Nonce случаен на каждую запись: одинаковые куки не дают одинаковых строк, по
// которым можно было бы сверить две базы между собой.
func TestSealIsRandomized(t *testing.T) {
	k := testKey(t)
	a, err := k.Seal("accounts:reserve", "одно и то же")
	if err != nil {
		t.Fatal(err)
	}
	b, err := k.Seal("accounts:reserve", "одно и то же")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("два шифрования одного значения совпали — nonce не случаен")
	}
}

// Шифротекст, переставленный в чужую строку, читаться не должен: контекст
// записи входит в проверку целостности.
func TestOpenRejectsForeignAAD(t *testing.T) {
	k := testKey(t)
	sealed, err := k.Seal(SessionAAD("telegram", 42), "куки")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Open(SessionAAD("telegram", 43), sealed); err == nil {
		t.Fatal("чужой контекст расшифровался")
	}
	if _, err := k.Open(AccountAAD("reserve"), sealed); err == nil {
		t.Fatal("шифротекст сессии прочитан как аккаунт")
	}
}

func TestOpenRejectsTampered(t *testing.T) {
	k := testKey(t)
	const aad = "accounts:reserve"
	sealed, err := k.Seal(aad, "куки")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sealed, Prefix))
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0x01
	broken := Prefix + base64.StdEncoding.EncodeToString(raw)
	if _, err := k.Open(aad, broken); err == nil {
		t.Fatal("испорченный шифротекст расшифровался")
	}
}

func TestOpenRejectsForeignKey(t *testing.T) {
	sealed, err := testKey(t).Seal("accounts:reserve", "куки")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testKey(t).Open("accounts:reserve", sealed); err == nil {
		t.Fatal("чужой ключ расшифровал значение")
	}
}

// Совместимость: записи, сделанные до включения шифрования, читаются и при
// включённом ключе, и без него.
func TestPlaintextPassesThrough(t *testing.T) {
	plain := `[{"name":"sid","value":"abc"}]`
	for _, tc := range []struct {
		name string
		key  Key
	}{
		{"без ключа", Key{}},
		{"с ключом", testKey(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.key.Open("sessions:telegram:1", plain)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if got != plain {
				t.Fatalf("открытая строка изменилась: %q", got)
			}
		})
	}
}

// Без ключа Seal отдаёт открытый текст: вызывающему не нужно ветвиться.
func TestSealWithoutKeyIsPlaintext(t *testing.T) {
	got, err := Key{}.Seal("accounts:reserve", "куки")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if got != "куки" {
		t.Fatalf("без ключа получено %q", got)
	}
}

// Ключ потеряли, а в базе шифротекст — это должно быть отдельной ошибкой, а не
// «сессии нет»: иначе демон молча попросит всех перелогиниться.
func TestOpenWithoutKeyIsErrNoKey(t *testing.T) {
	sealed, err := testKey(t).Seal("accounts:reserve", "куки")
	if err != nil {
		t.Fatal(err)
	}
	_, err = Key{}.Open("accounts:reserve", sealed)
	if !errors.Is(err, ErrNoKey) {
		t.Fatalf("ожидался ErrNoKey, получено: %v", err)
	}
}

func TestParse(t *testing.T) {
	if k, err := Parse(""); err != nil || k.Enabled() {
		t.Fatalf("пустая строка: ключ=%v err=%v", k.Enabled(), err)
	}
	if _, err := Parse("не base64!!"); err == nil {
		t.Fatal("кривой base64 принят")
	}
	short := base64.StdEncoding.EncodeToString(make([]byte, 16))
	if _, err := Parse(short); err == nil {
		t.Fatal("16-байтовый ключ принят вместо 32")
	}
}

func TestLoadFileAndPrecedence(t *testing.T) {
	b64, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "lovegw.key")
	// С переводом строки: файл почти наверняка создадут редактором.
	if err := os.WriteFile(path, []byte(b64+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k, err := Load("", path)
	if err != nil || !k.Enabled() {
		t.Fatalf("ключ из файла: включён=%v err=%v", k.Enabled(), err)
	}

	// Явное значение сильнее файла: env перебивает конфиг всюду в проекте.
	other, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	k, err = Load(other, path)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := k.Seal("accounts:reserve", "куки")
	if err != nil {
		t.Fatal(err)
	}
	fromFile, err := Parse(b64)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fromFile.Open("accounts:reserve", sealed); err == nil {
		t.Fatal("шифровали ключом из файла, а не явным")
	}

	if k, err := Load("", ""); err != nil || k.Enabled() {
		t.Fatalf("пустые источники: включён=%v err=%v", k.Enabled(), err)
	}
	if _, err := Load("", filepath.Join(t.TempDir(), "нет.key")); err == nil {
		t.Fatal("отсутствующий файл ключа принят молча")
	}
}
