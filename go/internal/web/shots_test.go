package web

import (
	"strings"
	"testing"

	"lovegw/internal/platform"
)

// Порядок иллюстраций задаёт ЯДРО (platform.mainImageFirst), и показ обязан его
// принять как есть: первая в списке — главная. Второе мнение здесь развело бы
// страницу с лентой и с каналом, которые выбирают картинку тем же правилом.
func TestMainShotIsTheFirstOneCoreGave(t *testing.T) {
	got := shotsView(312811, []platform.Media{
		{URL: "/media/aa/наша.webp"},
		{URL: "/media/bb/свежая-с-нгс.jpg"},
		{URL: "/media/cc/старая-с-нгс.jpg"},
	})
	if len(got) != 3 {
		t.Fatalf("картинок %d, ждали 3", len(got))
	}
	if got[0].URL != "/media/aa/наша.webp" {
		t.Errorf("главной оказалась %q", got[0].URL)
	}
}

// Строку, у которой известна одна ссылка, нарисовать нечем — и выброшена она
// ЗДЕСЬ, а не в шаблоне: полосу и стрелки решает число картинок, и считать оно
// обязано то, что читатель увидит. Иначе у заметки с одной живой картинкой и
// одной не забранной появились бы стрелки в никуда.
func TestShotWithoutBytesIsDropped(t *testing.T) {
	got := shotsView(312811, []platform.Media{
		{URL: "/media/aa/живая.webp"},
		{URL: ""},
	})
	if len(got) != 1 {
		t.Fatalf("картинок %d, ждали 1", len(got))
	}
	if got[0].Prev != "" || got[0].Next != "" {
		t.Error("у единственной картинки завелись стрелки: они вели бы на неё же")
	}
}

// Соседи считаются по КРУГУ: с последней «дальше» ведёт на первую. Круг, а не
// тупик, потому что просмотрщик у нас без счётчика — упёршись в стрелку, которая
// вдруг пропала, человек решил бы, что она сломалась.
func TestShotNeighboursGoInACircle(t *testing.T) {
	got := shotsView(312811, []platform.Media{
		{URL: "/1.jpg"}, {URL: "/2.jpg"}, {URL: "/3.jpg"},
	})
	if got[0].Anchor != "shot312811-0" || got[2].Anchor != "shot312811-2" {
		t.Fatalf("анкоры %q…%q", got[0].Anchor, got[2].Anchor)
	}
	if got[0].Prev != got[2].Anchor {
		t.Errorf("с первой назад ведёт %q, ждали %q", got[0].Prev, got[2].Anchor)
	}
	if got[2].Next != got[0].Anchor {
		t.Errorf("с последней вперёд ведёт %q, ждали %q", got[2].Next, got[0].Anchor)
	}
}

// На СТРАНИЦЕ это главная плюс полоса остальных плюс слой просмотрщика на
// каждую. Прежде все картинки выкладывались подряд во всю ширину, и заметка с
// четырьмя (313234) начиналась четырьмя экранами одного сюжета.
func TestNotePageShowsOneShotBigAndTheRestInARow(t *testing.T) {
	st := noteStore()
	st.images = []platform.Media{
		{URL: "/media/aa/главная.webp", Width: 1312, Height: 1199},
		{URL: "/media/bb/вторая.jpg"},
		{URL: "/media/cc/третья.jpg"},
	}
	body := do(openServer(t, st), guest(t, "GET", "/n/312811")).Body.String()

	if n := strings.Count(body, `class="shotmain"`); n != 1 {
		t.Errorf("главных картинок %d, ждали одну", n)
	}
	if n := strings.Count(body, `class="shotthumb"`); n != 2 {
		t.Errorf("картинок в полосе %d, ждали 2", n)
	}
	if n := strings.Count(body, `class="lbox"`); n != 3 {
		t.Errorf("слоёв просмотрщика %d, ждали 3", n)
	}
	if !strings.Contains(body, `href="#shot312811-0"`) {
		t.Error("с главной картинки нет входа в просмотрщик")
	}
	// Закрытие возвращает К КАРТИНКАМ, а не наверх страницы: человек смотрел
	// именно их и должен оказаться там же, где открыл.
	if !strings.Contains(body, `id="shots312811"`) || !strings.Contains(body, `href="#shots312811"`) {
		t.Error("«закрыть» некуда возвращать")
	}
}

// Одна картинка — ни полосы, ни стрелок: ряд из одного превью под ней самой и
// стрелки, ведущие на неё же, были бы мёртвыми кнопками. Просмотрщик при этом
// остаётся: увеличить картинку хотят и когда она одна.
func TestSingleShotHasNoRowAndNoArrows(t *testing.T) {
	st := noteStore()
	st.images = []platform.Media{{URL: "/media/aa/одна.webp", Width: 800, Height: 600}}
	body := do(openServer(t, st), guest(t, "GET", "/n/312811")).Body.String()

	if strings.Contains(body, `class="shotrow"`) {
		t.Error("полоса нарисована у единственной картинки")
	}
	if strings.Contains(body, "lbnav") {
		t.Error("стрелки нарисованы у единственной картинки")
	}
	if !strings.Contains(body, `class="lbox"`) {
		t.Error("просмотрщика нет: увеличить одну картинку тоже хотят")
	}
}
