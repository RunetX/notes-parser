package imgconv

// Что нам дали — по заголовку файла, без декодирования растра.
//
// Тип определяем СВОИМ разбором содержимого, а не заголовком Content-Type части
// multipart и не расширением имени: и то и другое пишет тот, кто присылает.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	_ "image/gif"  // размеры из заголовка
	_ "image/jpeg" // -//-
	_ "image/png"  // -//-
	"net/http"
	"strings"
)

// demuxers — какой демуксер ffmpeg назначаем каждому типу.
//
// Список закрытый, и это не придирка к форматам, а сокращение поверхности
// атаки: без явного -f ffmpeg перебирает ВСЕ демуксеры, и файл, назвавшийся
// картинкой, доедет до видеодекодера. Тип мы определили сами — сами и назначаем.
var demuxers = map[string]string{
	"image/jpeg": "jpeg_pipe",
	"image/png":  "png_pipe",
	"image/gif":  "gif_pipe",
	"image/webp": "webp_pipe",
}

// Inspect читает заголовок и решает, годится ли файл вообще.
func Inspect(data []byte) (Source, error) {
	if len(data) == 0 {
		return Source{}, ErrNotImage
	}
	if len(data) > MaxBytes {
		return Source{}, ErrTooBig
	}
	mime, _, _ := strings.Cut(http.DetectContentType(data), ";")
	if _, ok := demuxers[mime]; !ok {
		return Source{}, ErrNotImage
	}

	src := Source{MIME: mime}
	var err error
	if mime == "image/webp" {
		// stdlib webp не знает вовсе, а без размеров потолок точек не проверить:
		// lossless WebP 16000×16000 весит триста килобайт и разворачивается в
		// гигабайт. Поэтому свой разбор RIFF, а не «размеры по возможности».
		src.Width, src.Height, err = webpSize(data)
	} else {
		var cfg image.Config
		cfg, _, err = image.DecodeConfig(bytes.NewReader(data))
		src.Width, src.Height = cfg.Width, cfg.Height
	}
	if err != nil || src.Width <= 0 || src.Height <= 0 {
		return Source{}, ErrNotImage
	}
	if src.Width*src.Height > MaxPixels {
		return Source{}, ErrTooManyPx
	}
	if mime == "image/jpeg" {
		src.Orientation = exifOrientation(data)
	}
	return src, nil
}

// webpSize — размеры холста WebP. Три вида файла: расширенный (VP8X, там холст
// записан прямо), сжатый без потерь (VP8L, размеры в битовом потоке) и обычный
// (VP8 , размеры в заголовке ключевого кадра).
func webpSize(data []byte) (int, int, error) {
	if len(data) < 16 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return 0, 0, fmt.Errorf("не WebP")
	}
	// Идём по чанкам: четырёхбуквенный код, длина, содержимое (с выравниванием
	// до чётного). Первого достаточно — размеры холста лежат именно в нём.
	off := 12
	for off+8 <= len(data) {
		id := string(data[off : off+4])
		size := int(binary.LittleEndian.Uint32(data[off+4 : off+8]))
		body := data[off+8:]
		if size < 0 || size > len(body) {
			return 0, 0, fmt.Errorf("чанк %s обрезан", id)
		}
		body = body[:size]

		switch id {
		case "VP8X":
			if len(body) < 10 {
				return 0, 0, fmt.Errorf("VP8X обрезан")
			}
			w := int(body[4]) | int(body[5])<<8 | int(body[6])<<16
			h := int(body[7]) | int(body[8])<<8 | int(body[9])<<16
			return w + 1, h + 1, nil
		case "VP8L":
			if len(body) < 5 || body[0] != 0x2f {
				return 0, 0, fmt.Errorf("VP8L обрезан")
			}
			b := binary.LittleEndian.Uint32(body[1:5])
			return int(b&0x3fff) + 1, int((b>>14)&0x3fff) + 1, nil
		case "VP8 ":
			// Три байта метки кадра, затем стартовый код ключевого кадра.
			if len(body) < 10 || body[3] != 0x9d || body[4] != 0x01 || body[5] != 0x2a {
				return 0, 0, fmt.Errorf("VP8 без ключевого кадра")
			}
			w := int(binary.LittleEndian.Uint16(body[6:8]) & 0x3fff)
			h := int(binary.LittleEndian.Uint16(body[8:10]) & 0x3fff)
			return w, h, nil
		}
		off += 8 + size + size%2
	}
	return 0, 0, fmt.Errorf("в WebP нет чанка с размерами")
}
