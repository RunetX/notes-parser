package archive

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"image"
	_ "image/gif"  // регистрируем декодеры для image.Decode
	_ "image/jpeg" // аватары love.ngs.ru — 100×100 JPEG
	_ "image/png"
	"math/bits"
	"strings"
	"time"
)

// migrateV4SQL — перцептивные хэши аватаров (Фаза 2 распознавания личностей).
// Точное совпадение URL аватара мёртво (у каждой анкеты свой cache-хэш), но
// перцептивный dHash ловит визуально похожие фото (одно лицо в разных
// перезаливах/кропах) → сигнал avatar_phash в alias_candidates.
const migrateV4SQL = `
CREATE TABLE avatar_hashes (
    user_id    INTEGER PRIMARY KEY REFERENCES users(id),
    avatar_url TEXT NOT NULL,
    phash      INTEGER,               -- 64-битный dHash (int64-реинтерпретация); NULL при ошибке/дефолте
    kind       TEXT NOT NULL,         -- ok|default|error
    fetched_at TEXT NOT NULL
);
CREATE INDEX idx_avatar_kind  ON avatar_hashes(kind);
CREATE INDEX idx_avatar_phash ON avatar_hashes(phash);
`

// SignalAvatarPHash — сигнал связи по перцептивному хэшу аватара.
const SignalAvatarPHash = "avatar_phash"

// realAvatarURL: настоящий аватар (на CDN hsmedia), а не дефолтная заглушка
// love.ngs.ru/static/.../male300px.png|female300px.png.
func realAvatarURL(u string) bool {
	return strings.Contains(u, "hsmedia.ru") && strings.Contains(u, "/avatars/")
}

// AvatarTarget — пользователь с настоящим аватаром, подлежащий загрузке.
type AvatarTarget struct {
	UserID int64
	URL    string
}

// MarkDefaultAvatars помечает всех пользователей с дефолтной (не-CDN) аватаркой
// как kind='default' — чтобы не качать заглушки и честно считать покрытие.
// Идемпотентно (INSERT OR IGNORE). Возвращает число новых пометок.
func (s *Store) MarkDefaultAvatars(ctx context.Context, now time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO avatar_hashes (user_id, avatar_url, phash, kind, fetched_at)
		SELECT id, avatar_url, NULL, 'default', ?
		FROM users
		WHERE avatar_url NOT LIKE '%hsmedia.ru%avatars%'`, fmtTime(now))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// AvatarTargets — пользователи с настоящим аватаром, ещё не хэшированные
// (при refresh — все настоящие). limit>0 ограничивает выборку (для теста/смоука).
func (s *Store) AvatarTargets(ctx context.Context, refresh bool, limit int) ([]AvatarTarget, error) {
	q := `SELECT u.id, u.avatar_url FROM users u `
	if refresh {
		q += `WHERE u.avatar_url LIKE '%hsmedia.ru%avatars%'`
	} else {
		q += `LEFT JOIN avatar_hashes a ON a.user_id = u.id
		      WHERE a.user_id IS NULL AND u.avatar_url LIKE '%hsmedia.ru%avatars%'`
	}
	q += ` ORDER BY u.id`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AvatarTarget
	for rows.Next() {
		var t AvatarTarget
		if err := rows.Scan(&t.UserID, &t.URL); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SaveAvatarHash сохраняет результат по одному аватару (upsert). phash==nil —
// для kind error/default.
func (s *Store) SaveAvatarHash(ctx context.Context, userID int64, url string, phash *uint64, kind string, now time.Time) error {
	var pv any
	if phash != nil {
		pv = int64(*phash)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO avatar_hashes (user_id, avatar_url, phash, kind, fetched_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			avatar_url = excluded.avatar_url, phash = excluded.phash,
			kind = excluded.kind, fetched_at = excluded.fetched_at`,
		userID, url, pv, kind, fmtTime(now))
	return err
}

// AvatarHash — успешно посчитанный хэш аватара (для кластеризации).
type AvatarHash struct {
	UserID int64
	URL    string
	PHash  uint64
}

func (s *Store) avatarHashesOK(ctx context.Context) ([]AvatarHash, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT user_id, avatar_url, phash FROM avatar_hashes WHERE kind = 'ok' AND phash IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AvatarHash
	for rows.Next() {
		var h AvatarHash
		var p int64
		if err := rows.Scan(&h.UserID, &h.URL, &p); err != nil {
			return nil, err
		}
		h.PHash = uint64(p)
		out = append(out, h)
	}
	return out, rows.Err()
}

// AvatarClusterStats — итог ClusterAvatars.
type AvatarClusterStats struct {
	OK          int // хэшей ok в работе
	GenericHash int // exact-групп, признанных generic/дефолтом (пропущены)
	Skipped     int // пользователей в generic-группах (пропущены)
	Pairs       int // рёбер avatar_phash записано
}

// ClusterAvatars ищет пары визуально похожих аватаров (Hamming(dHash) ≤ maxDist)
// и пишет их в alias_candidates(signal=avatar_phash). Exact-группы размером
// больше genericMax считаются generic/дефолтными картинками (одна картинка у
// многих анкет — не альты) и пропускаются целиком. Идемпотентно (upsert).
func (s *Store) ClusterAvatars(ctx context.Context, maxDist, genericMax int, now time.Time) (AvatarClusterStats, error) {
	hs, err := s.avatarHashesOK(ctx)
	if err != nil {
		return AvatarClusterStats{}, err
	}
	st := AvatarClusterStats{OK: len(hs)}
	generic := genericHashes(hs, genericMax)
	st.GenericHash = len(generic)

	cand := make([]AvatarHash, 0, len(hs))
	for _, h := range hs {
		if generic[h.PHash] {
			st.Skipped++
			continue
		}
		cand = append(cand, h)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return st, err
	}
	defer tx.Rollback() //nolint:errcheck
	if st.Pairs, err = emitAvatarPairs(ctx, tx, cand, maxDist, fmtTime(now)); err != nil {
		return st, err
	}
	if err := tx.Commit(); err != nil {
		return st, err
	}
	return st, nil
}

// genericHashes — dHash-значения, встречающиеся у более чем genericMax анкет:
// это generic/дефолтные картинки (одно фото у многих), а не альты — пропускаем.
func genericHashes(hs []AvatarHash, genericMax int) map[uint64]bool {
	count := map[uint64]int{}
	for _, h := range hs {
		count[h.PHash]++
	}
	generic := map[uint64]bool{}
	for p, n := range count {
		if n > genericMax {
			generic[p] = true
		}
	}
	return generic
}

// emitAvatarPairs пишет рёбра для всех пар с Hamming(dHash) ≤ maxDist.
func emitAvatarPairs(ctx context.Context, tx *sql.Tx, cand []AvatarHash, maxDist int, nowStr string) (int, error) {
	pairs := 0
	for i := 0; i < len(cand); i++ {
		for j := i + 1; j < len(cand); j++ {
			d := bits.OnesCount64(cand[i].PHash ^ cand[j].PHash)
			if d > maxDist {
				continue
			}
			a, b := cand[i].UserID, cand[j].UserID
			if a > b {
				a, b = b, a
			}
			if a == b {
				continue
			}
			if err := upsertAliasCandidate(ctx, tx, aliasCand{
				a: a, b: b, signal: SignalAvatarPHash, score: avatarScore(d),
				evidence: fmt.Sprintf("аватар dHash расстояние %d", d),
			}, nowStr); err != nil {
				return pairs, err
			}
			pairs++
		}
	}
	return pairs, nil
}

// avatarScore переводит расстояние Хэмминга в вес связи: точная копия — высокий,
// далёкая почти-копия — ниже. Порог отсекается в ClusterPersonas по -min-score.
func avatarScore(dist int) float64 {
	switch {
	case dist == 0:
		return 0.9
	case dist <= 2:
		return 0.83
	case dist <= 4:
		return 0.75
	default:
		return 0.7
	}
}

// DHashFromBytes декодирует картинку и считает 64-битный difference hash (dHash):
// изображение ужимается до 9×8 в оттенках серого, каждый бит — «левый пиксель
// ярче правого». Устойчив к сжатию/масштабу, ловит визуально близкие фото.
func DHashFromBytes(b []byte) (uint64, error) {
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	return dHash(img), nil
}

const (
	dhashCols = 9 // 9 столбцов → 8 сравнений по горизонтали
	dhashRows = 8
)

func dHash(img image.Image) uint64 {
	bnd := img.Bounds()
	sw, sh := bnd.Dx(), bnd.Dy()
	if sw == 0 || sh == 0 {
		return 0
	}
	// Box-downscale в серую сетку 9×8: усредняем яркость по ячейкам.
	var sum [dhashRows][dhashCols]float64
	var cnt [dhashRows][dhashCols]int
	for y := 0; y < sh; y++ {
		ty := y * dhashRows / sh
		for x := 0; x < sw; x++ {
			tx := x * dhashCols / sw
			r, g, bl, _ := img.At(bnd.Min.X+x, bnd.Min.Y+y).RGBA()
			sum[ty][tx] += 0.299*float64(r) + 0.587*float64(g) + 0.114*float64(bl)
			cnt[ty][tx]++
		}
	}
	var h uint64
	bit := 0
	for ty := 0; ty < dhashRows; ty++ {
		for tx := 0; tx < dhashCols-1; tx++ {
			if cellAvg(sum[ty][tx], cnt[ty][tx]) > cellAvg(sum[ty][tx+1], cnt[ty][tx+1]) {
				h |= uint64(1) << uint(bit)
			}
			bit++
		}
	}
	return h
}

func cellAvg(s float64, c int) float64 {
	if c == 0 {
		return 0
	}
	return s / float64(c)
}

// AvatarCoverage — краткая сводка по avatar_hashes (для отчёта fetch/cluster).
type AvatarCoverage struct {
	OK, Default, Error int
}

func (s *Store) AvatarCoverage(ctx context.Context) (AvatarCoverage, error) {
	var c AvatarCoverage
	rows, err := s.db.QueryContext(ctx, `SELECT kind, COUNT(*) FROM avatar_hashes GROUP BY kind`)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			return c, err
		}
		switch kind {
		case "ok":
			c.OK = n
		case "default":
			c.Default = n
		case "error":
			c.Error = n
		}
	}
	return c, rows.Err()
}
