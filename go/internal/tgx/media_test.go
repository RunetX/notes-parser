package tgx

import (
	"testing"

	"github.com/go-telegram/bot/models"
)

// file_id привязан к типу вложения: один и тот же аватар уходит фотографией в
// канал и документом в комментарий, и кеш не должен подставлять photo-file_id
// в sendDocument — Telegram отвечает «can't use file of type Photo as Document»
// и комментарий теряет аватар.
func TestMediaCacheKeyedByKind(t *testing.T) {
	m := &Mirror{mediaCache: map[string]string{}, log: testLogger()}
	const url = "https://example.org/avatar.jpg"

	m.rememberFileID(mediaPhoto, url, "photo-fid")

	if in := m.mediaInput(mediaDocument, url, []byte("данные"), "avatar.jpg"); !isUpload(in) {
		t.Error("документ должен грузиться заново, а не брать photo-file_id")
	}
	in := m.mediaInput(mediaPhoto, url, []byte("данные"), "avatar.jpg")
	s, ok := in.(*models.InputFileString)
	if !ok || s.Data != "photo-fid" {
		t.Errorf("фото должно идти по кешу: %#v", in)
	}

	// Загрузив документом, кешируем отдельно — типы не затирают друг друга.
	m.rememberFileID(mediaDocument, url, "doc-fid")
	if d, ok := m.mediaInput(mediaDocument, url, nil, "avatar.jpg").(*models.InputFileString); !ok || d.Data != "doc-fid" {
		t.Error("документ должен идти по своему кешу")
	}
	if p, ok := m.mediaInput(mediaPhoto, url, nil, "avatar.jpg").(*models.InputFileString); !ok || p.Data != "photo-fid" {
		t.Error("photo-file_id затёрт документом")
	}
}

func isUpload(in models.InputFile) bool {
	_, ok := in.(*models.InputFileUpload)
	return ok
}
