package imgconv

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
)

// ---------------------------------------------------------------- Fit

func TestFitNeverEnlarges(t *testing.T) {
	cases := []struct {
		name  string
		src   Source
		wantW int
		wantH int
	}{
		{"меньше потолка — как было", Source{Width: 1000, Height: 800}, 1000, 800},
		{"ровно потолок", Source{Width: 1600, Height: 900}, 1600, 900},
		{"широкая", Source{Width: 3200, Height: 2400}, 1600, 1200},
		{"высокая", Source{Width: 2000, Height: 4000}, 800, 1600},
		{"квадрат", Source{Width: 3000, Height: 3000}, 1600, 1600},
		{"поворот меняет стороны", Source{Width: 4000, Height: 2000, Orientation: 6}, 800, 1600},
		{"поворот без масштаба", Source{Width: 800, Height: 600, Orientation: 8}, 600, 800},
		{"вырожденная полоса", Source{Width: 1, Height: 20000}, 1, 1600},
		{"пустые размеры", Source{}, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, h := Fit(c.src)
			if w != c.wantW || h != c.wantH {
				t.Fatalf("Fit = %d×%d, ожидалось %d×%d", w, h, c.wantW, c.wantH)
			}
		})
	}
}

// ---------------------------------------------------------------- EXIF

// jpegWithAPP1 собирает JPEG-каркас с готовым сегментом APP1.
func jpegWithAPP1(seg []byte) []byte {
	var b bytes.Buffer
	b.Write([]byte{0xff, 0xd8})
	if seg != nil {
		b.Write([]byte{0xff, 0xe1})
		_ = binary.Write(&b, binary.BigEndian, uint16(len(seg)+2))
		b.Write(seg)
	}
	b.Write([]byte{0xff, 0xd9})
	return b.Bytes()
}

// exifSeg собирает содержимое APP1 с одним тегом ориентации.
func exifSeg(big bool, orientation int) []byte {
	var ord binary.ByteOrder = binary.LittleEndian
	tag := "II"
	if big {
		ord = binary.BigEndian
		tag = "MM"
	}
	var b bytes.Buffer
	b.WriteString("Exif\x00\x00")
	b.WriteString(tag)
	u16 := func(v uint16) { _ = binary.Write(&b, ord, v) }
	u32 := func(v uint32) { _ = binary.Write(&b, ord, v) }
	u16(42)
	u32(8)     // IFD0 сразу за заголовком
	u16(1)     // одна запись
	u16(0x112) // ориентация
	u16(3)     // SHORT
	u32(1)
	u16(uint16(orientation))
	u16(0) // добивка значения до четырёх байт
	u32(0) // следующего IFD нет
	return b.Bytes()
}

func TestOrientationFromExif(t *testing.T) {
	for v := 1; v <= 8; v++ {
		for _, big := range []bool{false, true} {
			got := exifOrientation(jpegWithAPP1(exifSeg(big, v)))
			if got != v {
				t.Fatalf("порядок big=%v, ориентация %d: получили %d", big, v, got)
			}
		}
	}
}

func TestOrientationAbsentOrBroken(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"нет APP1", jpegWithAPP1(nil)},
		{"APP1 с XMP", jpegWithAPP1([]byte("http://ns.adobe.com/xap/1.0/\x00<x:xmpmeta/>"))},
		{"не JPEG", []byte("GIF89a")},
		{"пусто", nil},
		{"обрезанный сегмент", []byte{0xff, 0xd8, 0xff, 0xe1, 0x00, 0x40, 'E', 'x'}},
		{"порядок байт не тот", jpegWithAPP1(append([]byte("Exif\x00\x00XX"), make([]byte, 16)...))},
		{"смещение IFD за буфер", func() []byte {
			seg := exifSeg(false, 6)
			binary.LittleEndian.PutUint32(seg[6+4:6+8], 9999)
			return jpegWithAPP1(seg)
		}()},
		{"ориентация вне 1..8", jpegWithAPP1(exifSeg(false, 42))},
		{"мусор", bytes.Repeat([]byte{0xff}, 64)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := exifOrientation(c.data); got != 0 {
				t.Fatalf("ожидался 0, получили %d", got)
			}
		})
	}
}

func TestRotateFilterCoversEveryOrientation(t *testing.T) {
	want := map[int]string{
		0: "", 1: "", 2: "hflip", 3: "hflip,vflip", 4: "vflip",
		5: "transpose=0", 6: "transpose=1", 7: "transpose=3", 8: "transpose=2",
	}
	for o, w := range want {
		if got := rotateFilter(o); got != w {
			t.Fatalf("ориентация %d: фильтр %q, ожидался %q", o, got, w)
		}
	}
}

// ---------------------------------------------------------------- Inspect

func encJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func encPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func encGIF(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	img := image.NewPaletted(image.Rect(0, 0, w, h), color.Palette{color.Black, color.White})
	if err := gif.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// pngHeader — только сигнатура и IHDR: заголовка достаточно, чтобы объявить
// любые размеры, а весь растр рисовать ради теста незачем.
func pngHeader(w, h uint32) []byte {
	var ihdr bytes.Buffer
	ihdr.WriteString("IHDR")
	_ = binary.Write(&ihdr, binary.BigEndian, w)
	_ = binary.Write(&ihdr, binary.BigEndian, h)
	ihdr.Write([]byte{8, 6, 0, 0, 0}) // 8 бит, RGBA

	var b bytes.Buffer
	b.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	_ = binary.Write(&b, binary.BigEndian, uint32(ihdr.Len()-4))
	b.Write(ihdr.Bytes())
	_ = binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(ihdr.Bytes()))
	return b.Bytes()
}

func webpVP8L(w, h int) []byte {
	body := []byte{0x2f, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(body[1:5], uint32(w-1)|uint32(h-1)<<14)
	return riff("VP8L", body)
}

func webpVP8X(w, h int) []byte {
	body := make([]byte, 10)
	put3 := func(dst []byte, v int) {
		dst[0], dst[1], dst[2] = byte(v), byte(v>>8), byte(v>>16)
	}
	put3(body[4:], w-1)
	put3(body[7:], h-1)
	return riff("VP8X", body)
}

func webpVP8(w, h int) []byte {
	body := make([]byte, 10)
	body[3], body[4], body[5] = 0x9d, 0x01, 0x2a
	binary.LittleEndian.PutUint16(body[6:8], uint16(w))
	binary.LittleEndian.PutUint16(body[8:10], uint16(h))
	return riff("VP8 ", body)
}

func riff(id string, body []byte) []byte {
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(4+8+len(body)))
	b.WriteString("WEBP")
	b.WriteString(id)
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(body)))
	b.Write(body)
	return b.Bytes()
}

func TestInspectReadsEveryFormat(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		mime string
	}{
		{"jpeg", encJPEG(t, 120, 90), "image/jpeg"},
		{"png", encPNG(t, 120, 90), "image/png"},
		{"gif", encGIF(t, 120, 90), "image/gif"},
		{"webp lossless", webpVP8L(120, 90), "image/webp"},
		{"webp расширенный", webpVP8X(120, 90), "image/webp"},
		{"webp обычный", webpVP8(120, 90), "image/webp"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Заодно сверяем, что наш белый список совпадает с тем, как файл
			// опознаёт stdlib: разъедься они — и ffmpeg получит не тот демуксер.
			if mime, _, _ := strings.Cut(http.DetectContentType(c.data), ";"); mime != c.mime {
				t.Fatalf("DetectContentType = %q, ожидалось %q", mime, c.mime)
			}
			src, err := Inspect(c.data)
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			if src.MIME != c.mime || src.Width != 120 || src.Height != 90 {
				t.Fatalf("получили %s %d×%d", src.MIME, src.Width, src.Height)
			}
		})
	}
}

func TestInspectRefuses(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want error
	}{
		{"пусто", nil, ErrNotImage},
		{"страница геоблока", []byte("<html><body>403 Forbidden</body></html>"), ErrNotImage},
		{"текст", []byte("это просто письмо, а не картинка"), ErrNotImage},
		{"больше потолка", append(encPNG(t, 8, 8), bytes.Repeat([]byte{0}, MaxBytes)...), ErrTooBig},
		{"слишком много точек, png", pngHeader(20000, 20000), ErrTooManyPx},
		{"слишком много точек, webp", webpVP8L(16000, 16000), ErrTooManyPx},
		{"webp без чанка размеров", riff("ICCP", []byte{1, 2, 3, 4}), ErrNotImage},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Inspect(c.data)
			if !errors.Is(err, c.want) {
				t.Fatalf("Inspect вернул %v, ожидалось %v", err, c.want)
			}
		})
	}
}

func TestInspectReadsOrientation(t *testing.T) {
	// Настоящий JPEG с настоящим APP1: кодировщик stdlib пишет свой JFIF, наш
	// сегмент вставляем сразу за SOI.
	raw := encJPEG(t, 40, 30)
	seg := exifSeg(false, 6)
	var b bytes.Buffer
	b.Write(raw[:2])
	b.Write([]byte{0xff, 0xe1})
	_ = binary.Write(&b, binary.BigEndian, uint16(len(seg)+2))
	b.Write(seg)
	b.Write(raw[2:])

	src, err := Inspect(b.Bytes())
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if src.Orientation != 6 {
		t.Fatalf("ориентация %d, ожидалась 6", src.Orientation)
	}
	if w, h := Fit(src); w != 30 || h != 40 {
		t.Fatalf("Fit = %d×%d, ожидалось 30×40: поворот обязан менять стороны", w, h)
	}
}

func TestInspectIgnoresOrientationOutsideJPEG(t *testing.T) {
	if src, err := Inspect(encPNG(t, 10, 10)); err != nil || src.Orientation != 0 {
		t.Fatalf("у PNG ориентации быть не может: %v, %d", err, src.Orientation)
	}
}

// ---------------------------------------------------------------- ffmpeg

// recorder — шов внутрь пакета: записывает командную строку и отдаёт заданное.
type recorder struct {
	args [][]string
	out  []byte
	err  error
	text string
}

func (r *recorder) run(_ context.Context, _ string, args []string, _ []byte) ([]byte, string, error) {
	r.args = append(r.args, args)
	return r.out, r.text, r.err
}

func TestArgsAreTheBehaviour(t *testing.T) {
	rec := &recorder{out: []byte("webp")}
	f := &FFmpeg{Path: "/usr/local/bin/ffmpeg", codec: "webp", run: rec.run}

	if _, err := f.Convert(context.Background(), encJPEG(t, 3200, 1600)); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	want := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-max_alloc", "100000000",
		"-noautorotate",
		"-f", "jpeg_pipe",
		"-i", "pipe:0",
		"-map", "0:v:0", "-frames:v", "1",
		"-map_metadata", "-1",
		"-vf", "scale=1600:800:flags=lanczos",
		"-c:v", "libwebp", "-compression_level", "4", "-quality", "80", "-preset", "picture",
		"-f", "image2pipe", "pipe:1",
	}
	if !reflect.DeepEqual(rec.args[0], want) {
		t.Fatalf("командная строка разошлась.\nполучили:  %q\nожидалось: %q", rec.args[0], want)
	}
}

func TestArgsPutRotationBeforeScale(t *testing.T) {
	f := &FFmpeg{codec: "webp"}
	got := f.args(Source{MIME: "image/jpeg", Width: 4000, Height: 2000, Orientation: 6}, 1000, 1600)

	var vf string
	for i, a := range got {
		if a == "-vf" {
			vf = got[i+1]
		}
	}
	if vf != "transpose=1,scale=1000:1600:flags=lanczos" {
		t.Fatalf("фильтр %q: поворот обязан стоять ПЕРЕД масштабом", vf)
	}
}

func TestArgsFallBackToJPEG(t *testing.T) {
	rec := &recorder{out: []byte("jpeg")}
	f := &FFmpeg{codec: "jpeg", run: rec.run}
	res, err := f.Convert(context.Background(), encPNG(t, 100, 50))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if res.MIME != "image/jpeg" {
		t.Fatalf("MIME %q", res.MIME)
	}
	line := strings.Join(rec.args[0], " ")
	for _, want := range []string{"-f png_pipe", "-c:v mjpeg", "-pix_fmt yuvj420p"} {
		if !strings.Contains(line, want) {
			t.Fatalf("в строке нет %q: %s", want, line)
		}
	}
}

func TestConvertKeepsSizesItAskedFor(t *testing.T) {
	rec := &recorder{out: []byte("webp")}
	f := &FFmpeg{codec: "webp", run: rec.run}
	res, err := f.Convert(context.Background(), encPNG(t, 3000, 1500))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	// Размеры выхода не спрашиваются у выхода: их задали мы сами.
	if res.Width != 1600 || res.Height != 800 {
		t.Fatalf("размеры %d×%d", res.Width, res.Height)
	}
}

func TestConvertRefusesBadInputBeforeRunning(t *testing.T) {
	rec := &recorder{out: []byte("webp")}
	f := &FFmpeg{codec: "webp", run: rec.run}
	if _, err := f.Convert(context.Background(), []byte("<html>403</html>")); !errors.Is(err, ErrNotImage) {
		t.Fatalf("ожидался ErrNotImage, получили %v", err)
	}
	if len(rec.args) != 0 {
		t.Fatal("ffmpeg звали на файле, который картинкой не является")
	}
}

func TestConvertReportsStderr(t *testing.T) {
	long := strings.Repeat("ы", 500)
	rec := &recorder{err: fmt.Errorf("exit status 1"), text: long}
	f := &FFmpeg{codec: "webp", run: rec.run}

	_, err := f.Convert(context.Background(), encPNG(t, 10, 10))
	if !errors.Is(err, ErrConvert) {
		t.Fatalf("ожидался ErrConvert, получили %v", err)
	}
	if !strings.Contains(err.Error(), "…") {
		t.Fatalf("диагностика не обрезана: %v", err)
	}
}

func TestConvertRefusesEmptyOutput(t *testing.T) {
	rec := &recorder{}
	f := &FFmpeg{codec: "webp", run: rec.run}
	if _, err := f.Convert(context.Background(), encPNG(t, 10, 10)); !errors.Is(err, ErrConvert) {
		t.Fatalf("пустой поток обязан быть отказом, получили %v", err)
	}
}

func TestProbeChoosesCodec(t *testing.T) {
	t.Run("есть libwebp", func(t *testing.T) {
		rec := &recorder{out: []byte("файл")}
		f := &FFmpeg{run: rec.run}
		if err := f.Probe(context.Background()); err != nil || f.Codec() != "webp" {
			t.Fatalf("codec=%q err=%v", f.Codec(), err)
		}
		if len(rec.args) != 1 {
			t.Fatalf("проб было %d: вторая не нужна, когда сработала первая", len(rec.args))
		}
	})

	t.Run("нет libwebp — откат на jpeg", func(t *testing.T) {
		calls := 0
		f := &FFmpeg{run: func(_ context.Context, _ string, args []string, _ []byte) ([]byte, string, error) {
			calls++
			if strings.Contains(strings.Join(args, " "), "libwebp") {
				return nil, "Unknown encoder libwebp", fmt.Errorf("exit status 1")
			}
			return []byte("файл"), "", nil
		}}
		if err := f.Probe(context.Background()); err != nil || f.Codec() != "jpeg" {
			t.Fatalf("codec=%q err=%v", f.Codec(), err)
		}
		if calls != 2 {
			t.Fatalf("проб было %d, ожидалось 2", calls)
		}
	})

	t.Run("не умеет ничего", func(t *testing.T) {
		rec := &recorder{err: fmt.Errorf("exec format error")}
		f := &FFmpeg{run: rec.run}
		if err := f.Probe(context.Background()); err == nil {
			t.Fatal("ожидался отказ")
		}
		if f.Codec() != "" {
			t.Fatalf("codec=%q: не сумев ничего, пакет обязан сказать это пустотой", f.Codec())
		}
	})

	t.Run("пустой поток не считается успехом", func(t *testing.T) {
		rec := &recorder{}
		f := &FFmpeg{run: rec.run}
		if err := f.Probe(context.Background()); err == nil {
			t.Fatal("ожидался отказ: код возврата ноль, а байтов нет")
		}
	})
}

// TestFFmpegLive — настоящая конвертация настоящим бинарником. Пропускается без
// LOVEGW_TEST_FFMPEG, как pg-тесты без LOVEGW_TEST_PG_DSN.
func TestFFmpegLive(t *testing.T) {
	bin := os.Getenv("LOVEGW_TEST_FFMPEG")
	if bin == "" {
		t.Skip("LOVEGW_TEST_FFMPEG не задан")
	}
	f := &FFmpeg{Path: bin}
	if err := f.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	t.Logf("кодек сборки: %s", f.Codec())

	res, err := f.Convert(context.Background(), encPNG(t, 3000, 1500))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if res.Width != 1600 || res.Height != 800 {
		t.Fatalf("размеры %d×%d", res.Width, res.Height)
	}
	// Выход обязан быть тем, чем мы его назвали: иначе Caddy отдаст файл с
	// расширением, не совпадающим с содержимым.
	if mime, _, _ := strings.Cut(http.DetectContentType(res.Data), ";"); mime != res.MIME {
		t.Fatalf("на выходе %s, а обещали %s", mime, res.MIME)
	}
	src, err := Inspect(res.Data)
	if err != nil {
		t.Fatalf("выход не читается обратно: %v", err)
	}
	if src.Width != res.Width || src.Height != res.Height {
		t.Fatalf("в файле %d×%d, а в учёте %d×%d", src.Width, src.Height, res.Width, res.Height)
	}
	if src.Orientation != 0 {
		t.Fatalf("на выходе осталась ориентация %d: метаданные обязаны быть сняты", src.Orientation)
	}
}
