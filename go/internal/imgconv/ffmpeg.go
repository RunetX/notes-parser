package imgconv

// Перекодировщик поверх внешнего ffmpeg. Тот же приём, что в asr: вход и выход
// пайпами, временных файлов не создаётся вовсе — контейнер морды поднят
// read_only, а /tmp у него это tmpfs на 64 МБ, чьи страницы считаются той же
// памятью.

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	// stderrLimit — сколько символов диагностики тащить в текст ошибки.
	stderrLimit = 300

	// convertTimeout — сколько отпущено одному запуску. Секунда на картинку с
	// запасом в двадцать пять раз: если ffmpeg не уложился, дело не в размере.
	convertTimeout = 25 * time.Second

	// probeTimeout — проба на старте. Она не читает файлов и не считает ничего,
	// поэтому пять секунд здесь — это «бинарник не отвечает вовсе».
	probeTimeout = 5 * time.Second

	// maxAlloc — потолок одиночного av_malloc внутри ffmpeg, пояс поверх
	// подтяжек: потолок точек стоит раньше и ловит то же самое, но этот работает
	// и там, где заголовок соврал.
	maxAlloc = "100000000"
)

// runner — шов ВНУТРЬ пакета: им тест проверяет саму командную строку и разбор
// диагностики, не имея бинарника. Наружный шов — интерфейс Converter, и он для
// другого: им морда подделывает перекодировщик целиком.
type runner func(ctx context.Context, bin string, args []string, in []byte) (out []byte, errText string, err error)

// FFmpeg — перекодировщик поверх внешнего бинарника.
type FFmpeg struct {
	// Path — путь к бинарнику; пусто — ffmpeg из PATH. В образе задаётся
	// абсолютным путём: в distroless нет ни шелла, ни осмысленного PATH.
	Path string

	codec string
	run   runner
}

func (f *FFmpeg) bin() string {
	if f.Path == "" {
		return "ffmpeg"
	}
	return f.Path
}

func (f *FFmpeg) runner() runner {
	if f.run != nil {
		return f.run
	}
	return execRun
}

// Codec — что умеет эта сборка. Пусто до Probe и после неудачной пробы.
func (f *FFmpeg) Codec() string { return f.codec }

// Probe выясняет, чем перекодировать. Не разбором строки configuration и не
// таблицей -encoders: список говорит, что энкодер СОБРАН, а не что он работает,
// — а разбор человекочитаемой таблицы ломается вместе с её версткой. Поэтому
// звонок самому себе: закодировать чёрный квадрат и посмотреть, приехали ли
// байты.
func (f *FFmpeg) Probe(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	var last string
	for _, c := range []struct{ name, encoder string }{
		{"webp", "libwebp"},
		{"jpeg", "mjpeg"},
	} {
		args := []string{
			"-hide_banner", "-loglevel", "error", "-nostdin",
			"-f", "lavfi", "-i", "color=c=black:s=2x2",
			"-frames:v", "1", "-c:v", c.encoder,
			"-f", "image2pipe", "pipe:1",
		}
		out, errText, err := f.runner()(ctx, f.bin(), args, nil)
		if err == nil && len(out) > 0 {
			f.codec = c.name
			return nil
		}
		last = errText
		if err != nil && errText == "" {
			last = err.Error()
		}
	}
	f.codec = ""
	return fmt.Errorf("ffmpeg (%s) не умеет ни libwebp, ни mjpeg: %s", f.bin(), trim(last, stderrLimit))
}

// Convert приводит присланное к тому, что мы храним.
func (f *FFmpeg) Convert(ctx context.Context, in []byte) (Result, error) {
	src, err := Inspect(in)
	if err != nil {
		return Result{}, err
	}
	w, h := Fit(src)
	if w == 0 || h == 0 {
		return Result{}, ErrNotImage
	}

	ctx, cancel := context.WithTimeout(ctx, convertTimeout)
	defer cancel()

	out, errText, err := f.runner()(ctx, f.bin(), f.args(src, w, h), in)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v: %s", ErrConvert, err, trim(errText, stderrLimit))
	}
	if len(out) == 0 {
		return Result{}, fmt.Errorf("%w: пустой поток: %s", ErrConvert, trim(errText, stderrLimit))
	}
	mime := "image/webp"
	if f.codec == "jpeg" {
		mime = "image/jpeg"
	}
	return Result{Data: out, MIME: mime, Width: w, Height: h}, nil
}

// args — вся командная строка. Она и есть поведение пакета, поэтому тест
// сверяет её целиком.
func (f *FFmpeg) args(src Source, w, h int) []string {
	vf := fmt.Sprintf("scale=%d:%d:flags=lanczos", w, h)
	// Поворот СТРОГО перед масштабом: у ориентаций 5..8 стороны меняются
	// местами, и вписывать надо уже повёрнутое.
	if r := rotateFilter(src.Orientation); r != "" {
		vf = r + "," + vf
	}
	a := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-max_alloc", maxAlloc,
		// Наш поворот обязан быть единственным: сегодня ffmpeg по EXIF в JPEG
		// не крутит, но если завтрашний начнёт, мы повернём дважды.
		"-noautorotate",
		"-f", demuxers[src.MIME],
		"-i", "pipe:0",
		"-map", "0:v:0", "-frames:v", "1",
		// Снять всё, что приехало: место съёмки, время, камеру. libwebp
		// метаданных и так не пишет — это ремень поверх подтяжек.
		"-map_metadata", "-1",
		"-vf", vf,
	}
	if f.codec == "jpeg" {
		// yuvj420p не украшение: mjpeg не знает про прозрачность, и без явного
		// формата PNG с альфой уезжает в автосогласование.
		a = append(a, "-c:v", "mjpeg", "-q:v", "4", "-pix_fmt", "yuvj420p")
	} else {
		a = append(a, "-c:v", "libwebp",
			"-compression_level", "4",
			"-quality", strconv.Itoa(Quality),
			"-preset", "picture")
	}
	// image2pipe, а НЕ -f webp: муксер webp при закрытии умеет avio_seek (он
	// правит заголовок анимации), а stdout не перематывается. Пакет libwebp и
	// так является готовым файлом RIFF/WEBP целиком.
	return append(a, "-f", "image2pipe", "pipe:1")
}

// Version — первая строка -version, для doctor.
func (f *FFmpeg) Version(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, errText, err := f.runner()(ctx, f.bin(), []string{"-hide_banner", "-version"}, nil)
	if err != nil {
		return "", fmt.Errorf("ffmpeg (%s): %w: %s", f.bin(), err, trim(errText, stderrLimit))
	}
	line, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(line), nil
}

func execRun(ctx context.Context, bin string, args []string, in []byte) ([]byte, string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var out, errBuf bytes.Buffer
	if in != nil {
		cmd.Stdin = bytes.NewReader(in)
	}
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	// WaitDelay — чтобы зависший процесс не пережил свой контекст: убить его
	// после отмены больше некому, а слот семафора он держит.
	cmd.WaitDelay = 3 * time.Second
	err := cmd.Run()
	return out.Bytes(), errBuf.String(), err
}

func trim(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
