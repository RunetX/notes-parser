package asr

// Конвертация присланного аудио в WAV 16 кГц моно. Голосовое приходит как
// OGG/Opus, кружок — как MP4; ffmpeg читает stdin и пишет stdout, временные
// файлы не создаются.

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// ffmpegStderrLimit — сколько символов диагностики ffmpeg тащить в ошибку.
const ffmpegStderrLimit = 300

// FFmpeg — конвертер поверх внешнего бинарника.
type FFmpeg struct {
	// Path — путь к бинарнику; пусто — ffmpeg из PATH. В образе задаётся
	// абсолютным путём: в distroless нет ни шелла, ни осмысленного PATH.
	Path string
}

func (f *FFmpeg) bin() string {
	if f.Path == "" {
		return "ffmpeg"
	}
	return f.Path
}

// ToWAV конвертирует вход (OGG/Opus, MP4 и всё, что понимает ffmpeg) в
// WAV 16 кГц моно. -vn отбрасывает видеодорожку кружка.
func (f *FFmpeg) ToWAV(ctx context.Context, in []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, f.bin(),
		"-hide_banner", "-loglevel", "error",
		"-i", "pipe:0",
		"-vn", "-ac", "1", "-ar", "16000",
		"-f", "wav", "pipe:1")
	var out, errBuf bytes.Buffer
	cmd.Stdin = bytes.NewReader(in)
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %w: %s", err, trim(errBuf.String(), ffmpegStderrLimit))
	}
	if out.Len() == 0 {
		return nil, fmt.Errorf("ffmpeg вернул пустой поток: %s", trim(errBuf.String(), ffmpegStderrLimit))
	}
	return out.Bytes(), nil
}

// Check проверяет, что бинарник на месте и запускается (для doctor).
func (f *FFmpeg) Check(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, f.bin(), "-hide_banner", "-version").Output()
	if err != nil {
		return "", fmt.Errorf("ffmpeg (%s): %w", f.bin(), err)
	}
	line, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(line), nil
}

func trim(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
