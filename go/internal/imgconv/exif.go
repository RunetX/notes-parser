package imgconv

// Ориентация из EXIF — тег 0x0112 в IFD0.
//
// Читать её приходится нам: -autorotate у ffmpeg работает от матрицы поворота
// контейнера (MOV/MP4), а mjpeg-декодер кладёт EXIF в метаданные кадра, но сам
// кадр не крутит. Без этих строк каждая четвёртая фотография с телефона легла
// бы на страницу боком.
//
// Разбор нарочно куцый и не рекурсивный: идём по маркерам JPEG до первого APP1
// с «Exif\0\0», внутри читаем ТОЛЬКО IFD0 и ТОЛЬКО этот тег. Всё остальное,
// что умеет EXIF, нас не касается, а каждый лишний разобранный байт — это
// разбор чужого формата в нашей памяти.
//
// Чего не делаем: EXIF в чанке WebP (редкость) и ориентацию из XMP. Обратный
// риск — двойной поворот, если завтрашний ffmpeg начнёт крутить сам, — закрыт
// флагом -noautorotate, а не рассуждением о том, что он делает сегодня.

import "encoding/binary"

// exifOrientation — 1..8, либо 0, если тега нет. Ноль самый частый ответ: тег
// ставят камеры, а редакторы его снимают вместе с самим поворотом.
func exifOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return 0
	}
	for i := 2; i+4 <= len(data); {
		if data[i] != 0xff {
			return 0
		}
		marker := data[i+1]
		switch {
		case marker == 0xff: // байт-заполнитель перед маркером
			i++
			continue
		case marker == 0x01 || (marker >= 0xd0 && marker <= 0xd9):
			// Маркеры без содержимого; 0xda (начало данных) сюда не попадает,
			// но за ним EXIF всё равно не встречается.
			i += 2
			continue
		case marker == 0xda:
			return 0 // пошли сжатые данные, заголовков дальше нет
		}
		segLen := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
		if segLen < 2 || i+2+segLen > len(data) {
			return 0
		}
		if marker == 0xe1 {
			if o, ok := orientationFromAPP1(data[i+4 : i+2+segLen]); ok {
				return o
			}
		}
		i += 2 + segLen
	}
	return 0
}

// orientationFromAPP1 разбирает содержимое сегмента APP1.
func orientationFromAPP1(seg []byte) (int, bool) {
	const prefix = "Exif\x00\x00"
	if len(seg) < len(prefix)+8 || string(seg[:len(prefix)]) != prefix {
		return 0, false // бывает APP1 с XMP — он нам не нужен
	}
	tiff := seg[len(prefix):]

	var ord binary.ByteOrder
	switch string(tiff[0:2]) {
	case "II":
		ord = binary.LittleEndian
	case "MM":
		ord = binary.BigEndian
	default:
		return 0, false
	}
	if ord.Uint16(tiff[2:4]) != 42 {
		return 0, false
	}
	off := int(ord.Uint32(tiff[4:8]))
	if off < 8 || off+2 > len(tiff) {
		return 0, false
	}
	count := int(ord.Uint16(tiff[off : off+2]))
	off += 2
	for n := 0; n < count; n++ {
		if off+12 > len(tiff) {
			return 0, false
		}
		e := tiff[off : off+12]
		off += 12
		// Тип 3 — SHORT; значение короче четырёх байт лежит прямо в поле.
		if ord.Uint16(e[0:2]) != 0x0112 || ord.Uint16(e[2:4]) != 3 || ord.Uint32(e[4:8]) < 1 {
			continue
		}
		v := int(ord.Uint16(e[8:10]))
		if v >= 1 && v <= 8 {
			return v, true
		}
		return 0, false
	}
	return 0, false
}

// rotateFilter — фильтр ffmpeg, приводящий кадр к тому, как его снимали.
// Таблица стандартная; пустая строка означает «крутить нечего».
func rotateFilter(orientation int) string {
	switch orientation {
	case 2:
		return "hflip"
	case 3:
		return "hflip,vflip"
	case 4:
		return "vflip"
	case 5:
		return "transpose=0"
	case 6:
		return "transpose=1"
	case 7:
		return "transpose=3"
	case 8:
		return "transpose=2"
	default:
		return ""
	}
}
