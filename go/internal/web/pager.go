package web

// Постраничка номерами — та же, что на НГС: «« Пред. 1 2 3 4 5 6 7 … 5933 След. »».
//
// Курсорное пролистывание («ещё») устойчивее и дешевле, но здесь оно не годится
// принципиально: человек, читающий ленту третий год, знает, что он на странице
// 4, умеет вернуться на 3 и умеет прыгнуть на последнюю. Кнопка «ещё» отнимает
// и то, и другое, и назад с ней дороги нет. Поэтому номера — часть переноса
// привычки, а не украшение.

import "strconv"

// pageWindow — сколько номеров показывать подряд. Семь: ровно столько
// показывает НГС до многоточия.
const pageWindow = 7

// pageLink — одна позиция постранички. Gap — многоточие между окном и
// последней страницей: у него нет ни номера, ни ссылки.
//
// Far и Mob — про УЗКИЙ экран. Семь номеров со стрелками не помещаются в строку
// телефона (замер: 288 CSS-пикселей — это реальный viewport аппарата с крупным
// системным шрифтом), и постраничка уезжала второй строкой, разрывая «5867» и
// «След. »» от остальных. Ширины экрана сервер не знает и знать не может,
// поэтому решение принимает CSS, а разметка обязана НАЗВАТЬ, что прятать:
// Far — номер, который на телефоне не показывается, Mob — многоточие, которое
// на телефоне, наоборот, появляется вместо спрятанного хвоста.
type pageLink struct {
	Num     int
	URL     string
	Current bool
	Gap     bool
	Far     bool
	Mob     bool
}

type pager struct {
	Pages []pageLink
	Prev  string
	Next  string
	// Show — постраничку рисовать: на единственной странице она только мешает.
	Show bool
	// Total — всего страниц; Cur — нынешняя.
	Total int
	Cur   int
}

// newPager собирает постраничку. url — как построить адрес страницы N.
func newPager(cur, total int, url func(int) string) pager {
	if total < 1 {
		total = 1
	}
	if cur < 1 {
		cur = 1
	}
	if cur > total {
		cur = total
	}
	p := pager{Show: total > 1, Total: total, Cur: cur}
	if !p.Show {
		return p
	}
	if cur > 1 {
		p.Prev = url(cur - 1)
	}
	if cur < total {
		p.Next = url(cur + 1)
	}

	// Окно вокруг нынешней страницы, прижатое к краям: на первых страницах оно
	// начинается с единицы, на последних — упирается в конец.
	from := cur - pageWindow/2
	if from < 1 {
		from = 1
	}
	to := from + pageWindow - 1
	if to > total {
		to = total
		if from = to - pageWindow + 1; from < 1 {
			from = 1
		}
	}
	for n := from; n <= to; n++ {
		// На телефоне остаются нынешняя страница и две соседние: шаг влево-вправо
		// сохраняется, остальное делают стрелки. Первая и последняя не прячутся
		// никогда — прыжок в начало и в конец ленты это обычное движение.
		far := n != cur && n != 1 && n != total && (n-cur > 1 || cur-n > 1)
		p.Pages = append(p.Pages, pageLink{Num: n, URL: url(n), Current: n == cur, Far: far})
	}
	// Последняя страница видна всегда: прыжок в конец ленты — обычное движение,
	// и считать до неё стрелками невозможно.
	if to < total {
		if to < total-1 {
			p.Pages = append(p.Pages, pageLink{Gap: true})
		}
		p.Pages = append(p.Pages, pageLink{Num: total, URL: url(total)})
	}
	p.Pages = markMobileGaps(p.Pages)
	return p
}

// markMobileGaps дорисовывает многоточия для узкого экрана — там, где спрятанные
// номера разорвали бы подряд идущие. Без них «1 3 4 5» читается как поломка, а
// «1 2 8» — как «страниц восемь, и они подряд»: пропуск обязан быть видимым, и
// на широком экране ровно поэтому стоит своё многоточие.
//
// Проход идёт по УЗКОМУ виду (Far пропущены) и смотрит только на соседей: разрыв
// между номерами n и n+2 — это дыра, а уже стоящее многоточие второго не просит.
func markMobileGaps(in []pageLink) []pageLink {
	out := make([]pageLink, 0, len(in)+2)
	prev := -1 // номер последней видимой на узком экране страницы
	gapped := true
	for _, l := range in {
		switch {
		case l.Gap:
			prev, gapped = -1, true
		case l.Far:
			// Спрятана: следующей видимой понадобится многоточие.
			gapped = false
		default:
			if prev > 0 && l.Num > prev+1 && !gapped {
				out = append(out, pageLink{Gap: true, Mob: true})
			}
			prev, gapped = l.Num, true
		}
		out = append(out, l)
	}
	return out
}

// pageCount — сколько страниц выйдет из total строк по size на страницу.
func pageCount(total, size int) int {
	if total <= 0 || size <= 0 {
		return 1
	}
	n := (total + size - 1) / size
	if n < 1 {
		return 1
	}
	return n
}

// pageParam разбирает номер страницы из адреса. Мусор и ноль читаются как
// первая страница: постраничка приходит из ссылки, а ссылку правят руками.
func pageParam(s string) int {
	if s == "" {
		return 1
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0 // 0 — признак мусора, вызывающий ответит 400
	}
	return n
}
