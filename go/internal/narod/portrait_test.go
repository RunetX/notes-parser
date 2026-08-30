package narod

import (
	"strings"
	"testing"
)

// sevaBio — житель с приметой прямо в фактах: ровно тот случай, ради которого
// внешность спрашивают у модели, а не выводят кодом.
func sevaBio() Bio {
	return Bio{
		Nick:   "Дядя Стёпа с 9-го",
		Gender: "male",
		Age:    45,
		City:   "Новосибирск, Первомайский",
		Job:    "водитель автобуса, маршрут через полгорода",
		Family: "женат, трое взрослых детей",
		Facts: []string{
			"знает все пробки наизусть",
			"рост под два метра, отсюда и ник",
		},
		Frustration: FrustUnseen,
	}
}

// НИК И БОЛЬНОЕ МЕСТО НЕ УХОДЯТ МОДЕЛИ. Ник — потому что у слепка это имя
// живого человека, и портрет по нему был бы лицом настоящему участнику;
// frustration — по доводу PublicBio: это рычаг кубика, а не факт о человеке.
//
// Проверяется ЗАПРОС, а не намерение: список того, что модель видит, собирает
// одна функция, и тест стережёт именно её.
func TestЗапросПортретаНеЗнаетНиНикаНиБольного(t *testing.T) {
	b := sevaBio()
	system, prompt := PortraitRequest(b)
	both := system + "\n" + prompt

	for _, forbidden := range []string{b.Nick, b.Frustration, "Стёпа"} {
		if strings.Contains(both, forbidden) {
			t.Errorf("в запрос к модели уехало %q — этого она видеть не должна", forbidden)
		}
	}
	// А то, ради чего запрос и делается, в нём быть обязано.
	for _, want := range []string{"45", "male", "водитель автобуса", "рост под два метра"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("в запросе нет %q — по нему и рисуют", want)
		}
	}
}

// Семья в запрос тоже не идёт, и это не забывчивость: «женат, трое детей» на
// лице не видно, а место в промпте оно займёт. Модель просят писать только то,
// что увидел бы посторонний на снимке по плечи.
func TestЗапросПортретаБерётТолькоВидимое(t *testing.T) {
	b := sevaBio()
	_, prompt := PortraitRequest(b)
	if strings.Contains(prompt, b.Family) {
		t.Errorf("в запрос уехала семья: на снимке по плечи её не видно")
	}
}

// ЖРЕБИЙ КАМЕРЫ ПОВТОРЯЕМ. Промпт сохраняют файлом и гоняют по нему картинку
// много раз; «переснять» не должно означать «другой человек в другом кадре».
func TestКадрПовторяемИРазныйУРазных(t *testing.T) {
	b := sevaBio()
	first := Portrait(b, "dyadyastepa", 1, Look{}, false)
	again := Portrait(b, "dyadyastepa", 1, Look{}, false)
	if first != again {
		t.Error("два прогона дали разные промпты — сохранённый файл перестанет что-либо значить")
	}
	// Тот же человек под другим слугом — другой кадр: иначе тридцать портретов
	// выйдут паспортной серией.
	if other := Portrait(b, "kedrach", 1, Look{}, false); other == first {
		t.Error("у разных жителей совпал кадр целиком")
	}
	// И зерно должно двигать набор: им перебрасывают всю серию, если камера
	// вышла неудачной.
	if reseeded := Portrait(b, "dyadyastepa", 2, Look{}, false); reseeded == first {
		t.Error("зерно ничего не изменило — перебросить серию будет нечем")
	}
}

// Промпт называет возраст числом и пол словом: это единственные две величины,
// которые в карточке есть прямо, и угадывать их генератору не нужно.
func TestПромптНазываетВозрастИПол(t *testing.T) {
	got := Portrait(sevaBio(), "dyadyastepa", 1, Look{}, false)
	for _, want := range []string{"45-year-old", "Russian man", "fictional person"} {
		if !strings.Contains(got, want) {
			t.Errorf("в промпте нет %q:\n%s", want, got)
		}
	}
	// Ник в картинку не идёт тоже — он не про лицо, а надписей мы запрещаем.
	if strings.Contains(got, "Стёпа") {
		t.Error("ник уехал в промпт картинки")
	}
}

// Пол не назван — «person», а не «man». Мужчина «по умолчанию» назвал бы
// мужчинами половину площадки: та же ошибка, из-за которой у силуэта на
// странице заведён четвёртый, нейтральный.
func TestБезПолаПромптНеДодумывает(t *testing.T) {
	b := sevaBio()
	b.Gender = ""
	got := Portrait(b, "dyadyastepa", 1, Look{}, false)
	if strings.Contains(got, "Russian man") || strings.Contains(got, "Russian woman") {
		t.Errorf("пол додуман на пустом месте:\n%s", got)
	}
	if !strings.Contains(got, "Russian person") {
		t.Errorf("нейтрального оборота нет вовсе:\n%s", got)
	}
}

// Ответ модели встаёт на свои места, а пустая примета не оставляет дыры.
func TestОтветМоделиВстаётВПромпт(t *testing.T) {
	look := Look{
		Person:   "a very tall, heavy-set man with a broad flat face and thinning grey hair",
		Clothing: "a worn dark blue work jacket over a checked shirt",
	}
	got := Portrait(sevaBio(), "dyadyastepa", 1, look, false)
	if !strings.Contains(got, look.Person) {
		t.Error("внешность от модели не встала в промпт")
	}
	if !strings.Contains(got, "Wearing "+look.Clothing) {
		t.Error("одежда от модели не встала в промпт")
	}
	// Примета пуста — строки быть не должно, а не пустой строке с точкой.
	if strings.Contains(got, "\n.\n") || strings.Contains(got, ". \n") {
		t.Errorf("пустая примета оставила дыру:\n%s", got)
	}
}

// Хвост Midjourney дописывается только по флагу: в DALL·E и прочих он был бы
// мусором внутри промпта.
func TestХвостMidjourneyТолькоПоФлагу(t *testing.T) {
	b := sevaBio()
	if strings.Contains(Portrait(b, "dyadyastepa", 1, Look{}, false), "--ar") {
		t.Error("хвост Midjourney приехал без флага")
	}
	if !strings.Contains(Portrait(b, "dyadyastepa", 1, Look{}, true), "--ar 1:1") {
		t.Error("хвост Midjourney не приехал по флагу")
	}
}

// Возраст на лице просят ВСЕГДА и тем сильнее, чем человек старше: генераторы
// молодят и прихорашивают, а тридцать красивых лиц подряд выдают машину раньше
// любой стилистики.
func TestУВозрастаЕстьСледНаЛице(t *testing.T) {
	for _, c := range []struct {
		age  int
		want string
	}{{27, "everyday face"}, {37, "first lines"}, {49, "greying"}, {57, "deep lines"}} {
		b := sevaBio()
		b.Age = c.age
		got := Portrait(b, "dyadyastepa", 1, Look{}, false)
		if !strings.Contains(got, c.want) {
			t.Errorf("в %d лет промпт не просит %q", c.age, c.want)
		}
		if !strings.Contains(got, "not a model") {
			t.Errorf("в %d лет промпт не просит обычного лица", c.age)
		}
	}
}

// АНГЛИЙСКИЙ ПРОМПТА — И ЕСТЬ ПРОДУКТ, поэтому его склейка проверяется так же,
// как проверялась бы разметка страницы. Первый же прогон по боевым карточкам
// дал три огреха разом, и все три родились на стыке кода и таблицы: артикль
// внутри «Plain a soft off-white background», развилка «his or her age» и
// строчная буква после точки, потому что клауза модели приклеивалась хвостом к
// готовому предложению.
func TestПромптСклеенПоАнглийски(t *testing.T) {
	look := Look{
		Person:   "a very tall, heavy-set man with a broad flat face",
		Clothing: "a worn dark blue work jacket",
	}
	for _, slug := range []string{"dyadyastepa", "beret", "kedrach", "koshka", "kostik", "irma"} {
		got := Portrait(sevaBio(), slug, 1, look, false)
		for _, bad := range []string{"Plain a ", "his or her", ". a ", ". an "} {
			if strings.Contains(got, bad) {
				t.Errorf("%s: в промпте %q:\n%s", slug, bad, got)
			}
		}
	}
	// Местоимение согласовано с полом, а не выбирается генератором.
	if !strings.Contains(Portrait(sevaBio(), "dyadyastepa", 1, look, false), "He looks his age") {
		t.Error("мужчине не досталось своего местоимения")
	}
	w := sevaBio()
	w.Gender = "female"
	if !strings.Contains(Portrait(w, "beret", 1, look, false), "She looks her age") {
		t.Error("женщине не досталось своего местоимения")
	}
}
