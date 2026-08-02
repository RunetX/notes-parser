package asr

import (
	"context"
	"strings"
	"testing"
)

// Реальный ffmpeg в юнит-тестах не запускаем (в сервисе конвертер замокан):
// проверяем выбор бинарника и поведение, когда его нет.
func TestFFmpegBinary(t *testing.T) {
	if got := (&FFmpeg{}).bin(); got != "ffmpeg" {
		t.Errorf("по умолчанию берём ffmpeg из PATH, а не %q", got)
	}
	if got := (&FFmpeg{Path: "/usr/local/bin/ffmpeg"}).bin(); got != "/usr/local/bin/ffmpeg" {
		t.Errorf("путь из конфига: %q", got)
	}
}

func TestFFmpegMissingBinary(t *testing.T) {
	f := &FFmpeg{Path: "ffmpeg-которого-нет"}
	if _, err := f.ToWAV(context.Background(), []byte("OGG")); err == nil {
		t.Error("отсутствие бинарника должно быть ошибкой конвертации")
	}
	_, err := f.Check(context.Background())
	if err == nil {
		t.Fatal("Check должен ловить отсутствие бинарника")
	}
	if !strings.Contains(err.Error(), "ffmpeg-которого-нет") {
		t.Errorf("в ошибке нет пути к бинарнику: %v", err)
	}
}
