package archive

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"
	"unicode"
)

// migrateV14SQL — лексические профили авторов (TF-IDF по словам): второй сигнал
// атрибуции авторства рядом со стилометрией. lexis_profiles — на автора
// L2-нормированный вектор tf-idf в хэш-пространстве слов; lexis_meta (одна
// строка) — глобальный IDF-вектор, чтобы запрос взвешивался тем же IDF, что и
// профили. Слой производный и перестраиваемый (`personas lexis build`).
const migrateV14SQL = `
CREATE TABLE lexis_profiles (
    user_id  INTEGER PRIMARY KEY REFERENCES users(id),
    tokens   INTEGER NOT NULL,   -- сколько слов учтено (~объём текста)
    dims     INTEGER NOT NULL,   -- размерность хэш-вектора
    vec      BLOB NOT NULL,      -- dims × float32 LE, tf-idf, L2-нормированный
    built_at TEXT NOT NULL
);
CREATE TABLE lexis_meta (
    id       INTEGER PRIMARY KEY CHECK (id = 1),
    dims     INTEGER NOT NULL,
    docs     INTEGER NOT NULL,   -- N: авторов с профилем (для справки)
    idf      BLOB NOT NULL,      -- dims × float32 LE, IDF по корзинам слов
    built_at TEXT NOT NULL
);
`

// LexisBuildStats — итог BuildLexisProfiles.
type LexisBuildStats struct {
	Authors  int // авторов просмотрено
	Eligible int // профилей построено (слов ≥ minTokens)
}

// lexAcc — аккумулятор лексического профиля одного автора: сырые частоты слов
// в хэш-корзинах + счётчик слов.
type lexAcc struct {
	vec    []float32
	tokens int
}

// BuildLexisProfiles строит лексические TF-IDF-профили: проход по всему тексту
// автора (комментарии + заметки), накопление частот слов в хэш-пространстве,
// затем IDF по корзинам (в скольких авторах-документах встретилась корзина) и
// сублинейный tf. У кого ≥ minTokens слов — L2-нормированный tf-idf в
// lexis_profiles; IDF-вектор — в lexis_meta. Идемпотентно (перестраивает с нуля).
func (s *Store) BuildLexisProfiles(ctx context.Context, minTokens, dims int, now time.Time) (LexisBuildStats, error) {
	acc, err := s.accumulateLexis(ctx, dims)
	if err != nil {
		return LexisBuildStats{}, err
	}
	st := LexisBuildStats{Authors: len(acc)}

	idf, docs := lexisIDF(acc, minTokens, dims)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return st, err
	}
	defer tx.Rollback() //nolint:errcheck
	nowStr := fmtTime(now)
	if _, err := tx.ExecContext(ctx, `DELETE FROM lexis_profiles`); err != nil {
		return st, err
	}
	for uid, p := range acc {
		if p.tokens < minTokens {
			continue
		}
		prof := tfidfVec(p.vec, idf)
		if !l2Normalize(prof) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO lexis_profiles (user_id, tokens, dims, vec, built_at)
			VALUES (?, ?, ?, ?, ?)`,
			uid, p.tokens, dims, encodeVec(prof), nowStr); err != nil {
			return st, err
		}
		st.Eligible++
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO lexis_meta (id, dims, docs, idf, built_at) VALUES (1, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			dims = excluded.dims, docs = excluded.docs,
			idf = excluded.idf, built_at = excluded.built_at`,
		dims, docs, encodeVec(idf), nowStr); err != nil {
		return st, err
	}
	if err := tx.Commit(); err != nil {
		return st, err
	}
	return st, nil
}

// lexisIDF считает сглаженный IDF по корзинам слов: idf = ln((N+1)/(df+1)) + 1,
// где df — в скольких авторах-документах (≥ minTokens) корзина непуста, N — таких
// авторов. Возвращает вектор IDF и N.
func lexisIDF(acc map[int64]*lexAcc, minTokens, dims int) ([]float32, int) {
	df := make([]int, dims)
	docs := 0
	for _, p := range acc {
		if p.tokens < minTokens {
			continue
		}
		docs++
		for k, x := range p.vec {
			if x > 0 {
				df[k]++
			}
		}
	}
	idf := make([]float32, dims)
	for k := range idf {
		idf[k] = float32(math.Log(float64(docs+1)/float64(df[k]+1)) + 1)
	}
	return idf, docs
}

// tfidfVec переводит сырые частоты корзин в tf-idf: (1+ln tf)·idf на непустой
// корзине. Возвращает новый вектор (исходный не трогает).
func tfidfVec(raw, idf []float32) []float32 {
	v := make([]float32, len(raw))
	for k, x := range raw {
		if x > 0 {
			v[k] = float32(1+math.Log(float64(x))) * idf[k]
		}
	}
	return v
}

// accumulateLexis копит частоты слов на автора по всему его тексту (комментарии
// + заметки) — как accumulateStyle, но по словам, а не символьным 3-граммам.
func (s *Store) accumulateLexis(ctx context.Context, dims int) (map[int64]*lexAcc, error) {
	acc := map[int64]*lexAcc{}
	sources := []string{
		`SELECT author_id, text FROM comments WHERE author_id != 0`,
		`SELECT author_id, text FROM notes WHERE author_id IS NOT NULL AND author_id != 0`,
	}
	for _, q := range sources {
		if err := s.accumulateLexisFrom(ctx, q, dims, acc); err != nil {
			return nil, err
		}
	}
	return acc, nil
}

func (s *Store) accumulateLexisFrom(ctx context.Context, query string, dims int, acc map[int64]*lexAcc) error {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var aid int64
		var text string
		if err := rows.Scan(&aid, &text); err != nil {
			return err
		}
		p := acc[aid]
		if p == nil {
			p = &lexAcc{vec: make([]float32, dims)}
			acc[aid] = p
		}
		forEachWord(text, func(w []rune) {
			p.vec[int(hashWordRunes(w)%uint64(dims))]++
			p.tokens++
		})
	}
	return rows.Err()
}

// loadLexisProfiles грузит лексические профили и глобальный IDF. Если lexis-слой
// ещё не построен (нет строки meta), возвращает пустые срезы и dims=0 — вызов
// атрибуции трактует это как «сигнал недоступен».
func (s *Store) loadLexisProfiles(ctx context.Context) (ids []int64, vecs [][]float32, idf []float32, dims int, err error) {
	var idfBlob []byte
	err = s.db.QueryRowContext(ctx, `SELECT dims, idf FROM lexis_meta WHERE id = 1`).Scan(&dims, &idfBlob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil, 0, nil
	}
	if err != nil {
		return nil, nil, nil, 0, err
	}
	idf = decodeVec(idfBlob, dims)

	rows, err := s.db.QueryContext(ctx, `SELECT user_id, dims, vec FROM lexis_profiles`)
	if err != nil {
		return nil, nil, nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var uid int64
		var d int
		var blob []byte
		if err := rows.Scan(&uid, &d, &blob); err != nil {
			return nil, nil, nil, 0, err
		}
		ids = append(ids, uid)
		vecs = append(vecs, decodeVec(blob, d))
	}
	return ids, vecs, idf, dims, rows.Err()
}

// buildLexisQuery строит tf-idf-вектор текста запроса тем же IDF, что и профили.
// Возвращает L2-нормированный вектор, число слов (мера объёма) и признак
// непустого вектора (false — нормировать нечего).
func buildLexisQuery(text string, idf []float32, dims int) ([]float32, int, bool) {
	raw := make([]float32, dims)
	tokens := 0
	forEachWord(text, func(w []rune) {
		raw[int(hashWordRunes(w)%uint64(dims))]++
		tokens++
	})
	v := tfidfVec(raw, idf)
	ok := l2Normalize(v)
	return v, tokens, ok
}

// forEachWord вызывает fn на каждом слове текста: последовательности букв длиной
// ≥ 2, приведённые к нижнему регистру, ё→е. Буфер рун переиспользуется — fn НЕ
// должна его удерживать.
func forEachWord(s string, fn func(word []rune)) {
	var buf []rune
	flush := func() {
		if len(buf) >= 2 {
			fn(buf)
		}
		buf = buf[:0]
	}
	for _, r := range s {
		if !unicode.IsLetter(r) {
			flush()
			continue
		}
		lr := unicode.ToLower(r)
		if lr == 'ё' {
			lr = 'е'
		}
		buf = append(buf, lr)
	}
	flush()
}

// hashWordRunes — FNV-1a по байтам рун слова (те же константы, что hashTrigram).
func hashWordRunes(w []rune) uint64 {
	const (
		offset = 1469598103934665603
		prime  = 1099511628211
	)
	h := uint64(offset)
	for _, r := range w {
		v := uint32(r)
		for k := 0; k < 4; k++ {
			h ^= uint64(byte(v >> (k * 8)))
			h *= prime
		}
	}
	return h
}
