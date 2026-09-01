package web

// Поддельный вход: те же переходы, что у настоящего, но без Postgres и без
// похода на НГС. Проверять надо экраны и переходы между ними — SQL проверяют
// интеграционные тесты ядра, и гонять их ради страницы согласия незачем.

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"lovegw/internal/platform"
)

const (
	testProfileID = 1493279
	testNick      = "Рио"
	testInvite    = "T3H-INVI-TE00"
)

type fakeAuth struct {
	codes    map[int64]string            // выданный код по анкете (канал «о себе»)
	users    map[int64]platform.User     // кого знает площадка
	tokens   map[string]int64            // живые сессии
	consents map[int64]platform.Consents // что подписано
	invites  map[string]int64            // код → к кому привязан (0 — ни к кому)
	aborted  []int64                     // кому откатили вход
	revoked  map[int64][]string          // отозванные согласия
	avatars  map[int64]string            // фото в карточке участника
	botKeys  map[string]int64            // одноразовый ключ входа → чья анкета
	fail     error                       // если задано, всё падает этой ошибкой
}

func newFakeAuth() *fakeAuth {
	return &fakeAuth{
		codes:    map[int64]string{},
		users:    map[int64]platform.User{},
		tokens:   map[string]int64{},
		consents: map[int64]platform.Consents{},
		invites:  map[string]int64{testInvite: 0},
		revoked:  map[int64][]string{},
		avatars:  map[int64]string{},
		botKeys:  map[string]int64{},
	}
}

// Ключ гасится УДАЛЕНИЕМ, как в ядре: одноразовость держит отсутствие строки, а
// не отметка рядом с ней, — иначе подделка повторяла бы не то, что проверяет.
func (f *fakeAuth) RedeemBotLogin(_ context.Context, key string) (int64, error) {
	id, ok := f.botKeys[key]
	if !ok {
		return 0, platform.ErrBotKeyInvalid
	}
	delete(f.botKeys, key)
	return id, nil
}

func (f *fakeAuth) CompleteBotLogin(_ context.Context, userID int64) (int64, error) {
	if f.fail != nil {
		return 0, f.fail
	}
	u, ok := f.users[userID]
	if !ok {
		return 0, platform.ErrBotKeyInvalid
	}
	u.Kind = platform.KindMember
	f.users[userID] = u
	return userID, nil
}

func (f *fakeAuth) StartProfileChallenge(_ context.Context, id int64) (platform.Challenge, error) {
	if f.fail != nil {
		return platform.Challenge{}, f.fail
	}
	code := "T3H-CODE-" + strconv.FormatInt(id%10000, 10)
	f.codes[id] = code
	return platform.Challenge{Code: code, ExpiresAt: time.Now().Add(platform.ChallengeTTL)}, nil
}

func (f *fakeAuth) VerifyProfileChallenge(_ context.Context, id int64, code, aboutMe string) error {
	want, ok := f.codes[id]
	if !ok || code != want {
		return platform.ErrNoChallenge
	}
	if !strings.Contains(aboutMe, want) {
		return platform.ErrCodeNotFound
	}
	return nil
}

func (f *fakeAuth) CompleteNGSLogin(_ context.Context, prof platform.MirroredAuthor, g platform.Gender) (int64, error) {
	f.users[prof.ID] = platform.User{ID: prof.ID, Nick: prof.Nick, Kind: platform.KindMember}
	_ = g
	return prof.ID, nil
}

func (f *fakeAuth) AbortLogin(_ context.Context, userID int64) error {
	f.aborted = append(f.aborted, userID)
	delete(f.users, userID)
	for t, id := range f.tokens {
		if id == userID {
			delete(f.tokens, t)
		}
	}
	return nil
}

func (f *fakeAuth) RedeemInvite(_ context.Context, code, nick string) (int64, error) {
	bind, ok := f.invites[strings.ToUpper(strings.TrimSpace(code))]
	if !ok {
		return 0, platform.ErrInviteInvalid
	}
	if bind == 0 {
		bind = platform.NativeIDBase + 7
	}
	f.users[bind] = platform.User{ID: bind, Nick: nick, Kind: platform.KindMember}
	delete(f.invites, code)
	return bind, nil
}

func (f *fakeAuth) CreateSession(_ context.Context, userID int64, _ string) (string, time.Time, error) {
	token := "tok" + strconv.FormatInt(userID, 10)
	f.tokens[token] = userID
	return token, time.Now().Add(platform.SessionTTL), nil
}

func (f *fakeAuth) SessionUser(_ context.Context, token string) (platform.User, error) {
	if f.fail != nil {
		return platform.User{}, f.fail
	}
	id, ok := f.tokens[token]
	if !ok {
		return platform.User{}, platform.ErrNotFound
	}
	u, ok := f.users[id]
	if !ok {
		return platform.User{}, platform.ErrNotFound
	}
	return u, nil
}

func (f *fakeAuth) RevokeSession(_ context.Context, token string) error {
	delete(f.tokens, token)
	return nil
}

func (f *fakeAuth) MemberCard(_ context.Context, id int64) (platform.Author, error) {
	u, ok := f.users[id]
	if !ok {
		return platform.Author{}, platform.ErrNotFound
	}
	return platform.Author{ID: u.ID, Nick: u.Nick, AvatarURL: f.avatars[id]}, nil
}

func (f *fakeAuth) MissingConsent(_ context.Context, userID int64, op platform.Operator) (platform.ConsentDoc, error) {
	docs, err := platform.CurrentConsentDocs(op)
	if err != nil {
		return platform.ConsentDoc{}, err
	}
	have := f.consents[userID]
	for _, d := range docs {
		if !have.Has(d.Kind, d.Version) {
			return d, nil
		}
	}
	return platform.ConsentDoc{}, nil
}

func (f *fakeAuth) UserConsents(_ context.Context, userID int64) (platform.Consents, error) {
	return f.consents[userID], nil
}

func (f *fakeAuth) GrantConsent(_ context.Context, userID int64, kind string, version int, _ string) error {
	if f.consents[userID] == nil {
		f.consents[userID] = platform.Consents{}
	}
	f.consents[userID][kind] = platform.ConsentRecord{
		Kind: kind, Version: version, GrantedAt: time.Now(),
	}
	return nil
}

func (f *fakeAuth) RevokeConsent(_ context.Context, userID int64, kind string) error {
	f.revoked[userID] = append(f.revoked[userID], kind)
	now := time.Now()
	for k, rec := range f.consents[userID] {
		if k == kind || kind == platform.ConsentProcessing {
			rec.RevokedAt = &now
			f.consents[userID][k] = rec
		}
	}
	return nil
}

// fakeSite — анкета НГС без похода на НГС. Только чтение: слать код в личку
// площадка больше не умеет (см. шапку platform/auth.go).
type fakeSite struct {
	prof      SiteProfile
	missing   bool
	err       error
	avatarErr error // НГС не отдал файл по ссылке из анкеты
}

func (s *fakeSite) Profile(context.Context, int64) (SiteProfile, error) {
	switch {
	case s.missing:
		return SiteProfile{}, ErrNoProfile
	case s.err != nil:
		return SiteProfile{}, s.err
	}
	return s.prof, nil
}

// Avatar — байты «файла» с CDN. Что именно приехало, тесту неважно: картинку от
// заглушки отличает хранилище (platform.MediaStore), а не морда.
func (s *fakeSite) Avatar(_ context.Context, url string) ([]byte, error) {
	if s.avatarErr != nil {
		return nil, s.avatarErr
	}
	return []byte("байты " + url), nil
}

// grantConsents подписывает за человека ДЕЙСТВУЮЩИЕ редакции обоих документов.
// Версия берётся из самих текстов, а не пишется числом: опубликованная редакция
// неизменяема, поэтому правка документа — всегда новая версия, и захардкоженная
// единица роняла бы тесты, к согласиям отношения не имеющие.
func grantConsents(t *testing.T, auth *fakeAuth, userID int64) {
	t.Helper()
	docs, err := platform.CurrentConsentDocs(platform.Operator{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		if err := auth.GrantConsent(context.Background(), userID, d.Kind, d.Version, ""); err != nil {
			t.Fatal(err)
		}
	}
}

// consentVersion — номер ДЕЙСТВУЮЩЕЙ редакции документа, каким его ждёт форма.
// Хардкод «1» в тестах означал бы, что первая же новая редакция ломает не
// проверку версий, а весь сценарий входа.
func consentVersion(t *testing.T, kind string) string {
	t.Helper()
	docs, err := platform.CurrentConsentDocs(platform.Operator{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		if d.Kind == kind {
			return strconv.Itoa(d.Version)
		}
	}
	t.Fatalf("нет действующей редакции %s", kind)
	return ""
}
