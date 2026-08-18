package web

// Смайлы НГС: :::popcorn:::, :::boogi:::, :::agree:::.
//
// Своя система знаков, дожившая до сентября 2017-го, и в архиве она заметнее
// BB-кодов: замер 18.08.2026 по 1 642 652 комментариям выгрузок досье — 6–11 %
// комментариев 2013–2017 годов несут хотя бы один смайл, у одного
// «:::popcorn:::» 9 581 вхождение. Без разбора это голые коды посреди фразы.
//
// Картинки вшиты в бинарник (assets/smile), а не сложены в хранилище медиа:
// это часть вида страницы, а не чьё-то вложение. Скачаны они с самого сайта
// 18.08.2026 — и совпали БАЙТ В БАЙТ с копией 2015 года из старого парсера
// (dump/lnParser), то есть за одиннадцать лет ни один файл не поменялся.
// Восемь кодов, встреченных в архиве, сайт не отдаёт вовсе (baby2, bye,
// chereshnya, cofee, coo1, cpool1, eys1, yawn — от 1 до 4 вхождений каждый):
// это опечатки авторов, и на самом НГС они тоже остались текстом.
//
// Окно закрывается вместе с сайтом: пока love.ngs.ru отвечает, файлы можно
// забрать, потом — неоткуда. Поэтому взят ВЕСЬ набор, а не только встреченные
// в выгрузках коды: полный корпус вшестеро больше замера, и там наверняка есть
// коды, которых мы не видели. Восемь файлов (baby, cake, coffee, blush1, eek1,
// smile1, tongue1, yes) найдены перебором соседних имён — каталог сайта не
// листается, а страница-выбиралка смайлов с него давно снята. Итого 64 файла,
// 147 КБ.

import (
	"html"
	"html/template"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// smileyRe — код смайла в тексте. Нижний регистр и цифры: так их писал сайт
// (`:::cool1:::`, `:::heart2:::`), а верхний регистр в выгрузках не встречается.
var smileyRe = regexp.MustCompile(`:::([a-z0-9_]{1,20}):::`)

// smileySunset — когда сайт перестал их показывать. Замер тот же, что у
// bbSunset: коды идут сплошь до 08.2017, в сентябре их 11, дальше НОЛЬ на
// 1,3 млн комментариев 2018–2026 годов. Разметка и смайлы умерли порознь —
// отсюда и два рубежа вместо одного «раньше было лучше».
var smileySunset = time.Date(2017, 10, 1, 0, 0, 0, 0, time.UTC)

// smiley — картинка и её собственный размер. Размеры у набора разные (от 15×15
// до 100×20), поэтому они читаются из самого файла и ставятся атрибутами: без
// них строка дёргается, пока грузится картинка.
type smiley struct {
	url string
	w   int
	h   int
}

var smiles = map[string]smiley{}

// initSmiles вызывается из init статики, а не своим init: порядок здесь не
// вопрос вкуса — карта строится ИЗ уже собранных assetURLs, а порядок двух
// init в пакете задаётся именами файлов, то есть держится случайностью.
func initSmiles() {
	for name, url := range assetURLs {
		if !strings.HasPrefix(name, "smile/") || path.Ext(name) != ".gif" {
			continue
		}
		code := strings.TrimSuffix(path.Base(name), ".gif")
		s := smiley{url: url}
		if a, ok := assets[strings.TrimPrefix(url, "/assets/")]; ok {
			s.w, s.h = gifSize(a.data)
		}
		smiles[code] = s
	}
	if len(smiles) == 0 {
		panic("web: картинки смайлов не найдены")
	}
}

// gifSize достаёт размер из заголовка GIF: после «GIF89a» идут ширина и высота
// по два байта, младшим вперёд. Читать его самим дешевле, чем держать рядом
// таблицу размеров, которая разъедется с файлами при первой же замене.
func gifSize(b []byte) (int, int) {
	if len(b) < 10 || string(b[:3]) != "GIF" {
		return 0, 0
	}
	return int(b[6]) | int(b[7])<<8, int(b[8]) | int(b[9])<<8
}

// smileImg — картинка смайла по коду. Нужна не только тексту: теми же знаками
// подписаны кнопки реакций (react.go), и рисоваться они обязаны одинаково —
// иначе «:::agree:::» в реплике и «согласен» на кнопке выглядели бы разной
// мыслью. Неизвестный код даёт пустоту, а не битую картинку.
func smileImg(code string) template.HTML {
	s, ok := smiles[code]
	if !ok {
		return ""
	}
	var b strings.Builder
	writeSmileImg(&b, s, ":::"+code+":::")
	return template.HTML(b.String())
}

// writeSmileys пишет текст, подменяя известные коды картинками.
//
// Экранирование остаётся первичным: подставляем мы только СВОИ адреса из
// статики, а всё, что пришло из базы, уходит в html.EscapeString — в том числе
// сам код в alt. Неизвестный код не разметка и не ошибка: он остаётся текстом,
// ровно как оставался на сайте.
func writeSmileys(b *strings.Builder, line string) {
	idx := 0
	for _, m := range smileyRe.FindAllStringSubmatchIndex(line, -1) {
		s, ok := smiles[line[m[2]:m[3]]]
		if !ok {
			continue
		}
		b.WriteString(html.EscapeString(line[idx:m[0]]))
		writeSmileImg(b, s, line[m[0]:m[1]])
		idx = m[1]
	}
	b.WriteString(html.EscapeString(line[idx:]))
}

// writeSmileImg — сама картинка. Размер ставится атрибутами: набор
// разнокалиберный, и без них строка дёргается, пока смайлы грузятся.
func writeSmileImg(b *strings.Builder, s smiley, alt string) {
	b.WriteString(`<img class="sm" src="`)
	b.WriteString(s.url)
	b.WriteString(`" alt="`)
	b.WriteString(html.EscapeString(alt))
	if s.w > 0 {
		b.WriteString(`" width="` + strconv.Itoa(s.w) + `" height="` + strconv.Itoa(s.h))
	}
	b.WriteString(`" loading="lazy">`)
}
