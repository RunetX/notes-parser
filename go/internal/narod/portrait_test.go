package narod

import (
	"fmt"
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
		Face:     "a broad flat face with a crooked nose and thinning grey hair",
		Clothing: "a worn dark blue work jacket over a checked shirt",
	}
	got := Portrait(sevaBio(), "dyadyastepa", 1, look, false)
	if !strings.Contains(got, look.Face) {
		t.Error("лицо от модели не встало в промпт")
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

// ВОЗРАСТ — ЯКОРЬ В ОБЕ СТОРОНЫ, а не толчок в одну.
//
// Первая редакция просила только «клади на лицо годы», из верного наблюдения,
// что генераторы молодят. Поправка не знала меры и складывалась с тремя другими
// просьбами про невзрачность: у 33-летней воспитательницы вышло лицо на сорок
// пять (жалоба владельца, 30.08.2026). Теперь названы ОБЕ стороны — по тому же
// доводу, что у односложности реплик: где инструкции нет, модель делает то же,
// что делала, и умолчание оказывается сильнее просьбы.
func TestВозрастЗакреплёнВОбеСтороны(t *testing.T) {
	for _, age := range []int{25, 33, 38, 47, 58} {
		b := sevaBio()
		b.Age = age
		got := Portrait(b, "dyadyastepa", 1, Look{}, false)
		want := fmt.Sprintf("exactly %d — no older and no younger", age)
		if !strings.Contains(got, want) {
			t.Errorf("в %d лет промпт не закрепляет возраст (%q):\n%s", age, want, got)
		}
	}
	// Молодому лицу возрастных примет не назначают вовсе.
	young := sevaBio()
	young.Age = 27
	got := Portrait(young, "dyadyastepa", 1, Look{}, false)
	for _, bad := range []string{"grey hair", "lines", "weathered"} {
		if strings.Contains(got, bad) {
			t.Errorf("двадцатисемилетнему приписали %q:\n%s", bad, got)
		}
	}
	// А про обычность лица сказано РОВНО ОДИН РАЗ: три синонимичные просьбы и
	// дали «потрёпанного» вместо «обычного».
	if n := strings.Count(got, "not a model"); n != 1 {
		t.Errorf("про обычность лица сказано %d раз, ожидался один", n)
	}
	for _, gone := range []string{"pores and blemishes", "no retouching"} {
		if strings.Contains(got, gone) {
			t.Errorf("лишняя просьба про невзрачность: %q", gone)
		}
	}
}

// СЛОЖЕНИЕ БРОСАЕТ ЖРЕБИЙ, а не выбирает модель.
//
// Оплачено боем: в задании модели стояло «дай настоящие лица: ВЕС, редеющие
// волосы, кривые зубы — это важнее всего остального», и у пятерых жителей из
// пяти вышло полное сложение. Список в промпте модель читает как рецепт и
// исполняет весь — та же грабля, что у jabsLine.
func TestСложениеБросаетЖребий(t *testing.T) {
	seen := map[string]int{}
	slugs := []string{
		"beret", "dyadyastepa", "gosha", "hlopushka", "irma", "kedrach",
		"koshka", "kostik", "kuzmich", "lisenok", "mazay", "myatnaya",
		"nyurka", "olgabat", "pelmen", "polkovnik", "professor", "prorab",
	}
	for _, slug := range slugs {
		got := Portrait(sevaBio(), slug, 1, Look{}, false)
		for _, b := range portraitBuilds {
			if strings.Contains(got, b) {
				seen[b]++
				break
			}
		}
	}
	if len(seen) < 4 {
		t.Errorf("на восемнадцати жителях сложений всего %d: %v", len(seen), seen)
	}
	// Полных не должно быть большинством — ровно то, с чего началась жалоба.
	heavy := seen["a broad, heavy build"] + seen["a stocky, solid build"]
	if heavy*2 > len(slugs) {
		t.Errorf("плотных сложений %d из %d — это снова «все упитанные»", heavy, len(slugs))
	}
}

// Модель про сложение не спрашивают вовсе, и запрет стоит в задании явно.
func TestМоделиЗапрещеноСложение(t *testing.T) {
	system, _ := PortraitRequest(sevaBio())
	for _, want := range []string{"BODY SIZE IS ALREADY DECIDED", "AGE IS ALREADY STATED"} {
		if !strings.Contains(system, want) {
			t.Errorf("в задании модели нет запрета %q", want)
		}
	}
	if strings.Contains(system, "weight, thinning hair") {
		t.Error("в задании модели остался список, из-за которого все вышли упитанными")
	}
}

// ГРАНИЦА ЗАГАРА ЗАПРЕЩЕНА, и запрет стоит в задании модели.
//
// Оплачено боем 30.08.2026: владелец увидел на аватарке «строгий переход загара,
// будто часть не загорела под каской». Так и было — модель писала это сама, семь
// приметок из тридцати про солнце: «deep tan line across the forehead where a
// cap sits, paler skin above». Генератор рисует такую границу ЖЁСТКОЙ ПОЛОСОЙ, и
// на квадрате 100x100 ничего, кроме полосы, не видно.
func TestГраницаЗагараЗапрещена(t *testing.T) {
	system, _ := PortraitRequest(sevaBio())
	for _, want := range []string{"NEVER a boundary between two skin tones", "no tan lines", "hard stripe"} {
		if !strings.Contains(system, want) {
			t.Errorf("в задании модели нет запрета %q", want)
		}
	}
}

// ПРИМЕТА ОБЯЗАНА ПОМЕЩАТЬСЯ В КАДР. Восемь примет из тридцати оказались на
// руках — костяшки, ногти, запястья, предплечья, — а последняя строка того же
// промпта говорит «no hands in frame». Промпт противоречил сам себе, и
// генератор разрешал противоречие как умел.
func TestПриметаНеУезжаетИзКадра(t *testing.T) {
	system, _ := PortraitRequest(sevaBio())
	for _, want := range []string{"HEAD-AND-SHOULDERS crop", "Never hands, knuckles, nails"} {
		if !strings.Contains(system, want) {
			t.Errorf("в задании модели нет запрета %q", want)
		}
	}
}

// ПРИМЕТА ВЫПАДАЕТ ЖРЕБИЕМ, а не стоит у каждого. Модель на «верни пустую
// строку, если приметы нет» отвечала приметой в 28 случаях из 30 — то есть
// придумывала, а «особинка у каждого встречного» выдаёт машину так же, как
// одинаковое сложение.
func TestПриметаВыпадаетЖребием(t *testing.T) {
	look := Look{Face: "a plain face", Clothing: "a grey jacket", Detail: "a small scar on the jaw"}
	slugs := []string{
		"beret", "dyadyastepa", "gosha", "hlopushka", "irma", "kedrach",
		"koshka", "kostik", "kuzmich", "lisenok", "mazay", "myatnaya",
		"nyurka", "olgabat", "pelmen", "polkovnik", "professor", "prorab",
		"ryabina", "ryabinka", "sansanych", "seva", "shtangen", "sovushka",
		"svarnoy", "taygafm", "tetyamotya", "valpetrovna", "vesnushka", "vishnya",
	}
	with := 0
	for _, slug := range slugs {
		if strings.Contains(Portrait(sevaBio(), slug, 1, look, false), "small scar on the jaw") {
			with++
		}
	}
	if with == 0 || with == len(slugs) {
		t.Fatalf("примета есть у %d жителей из %d — жребий не бросается вовсе", with, len(slugs))
	}
	// Доля не проверяется точно (это бросок), но «почти у всех» — это ровно то
	// состояние, из-за которого монетка и заведена.
	if with*2 > len(slugs) {
		t.Errorf("примета у %d из %d — снова особинка у каждого встречного", with, len(slugs))
	}
}

// Монетка приметы НЕ СДВИГАЕТ КАМЕРУ: у неё своя соль, и кадр у жителя тот же,
// какой был до её появления. Иначе тридцать готовых промптов сменили бы ракурс
// без всякого повода.
func TestМонеткаПриметыНеТрогаетКадр(t *testing.T) {
	bare := Portrait(sevaBio(), "dyadyastepa", 1, Look{}, false)
	withDetail := Portrait(sevaBio(), "dyadyastepa", 1,
		Look{Detail: "a small scar on the jaw"}, false)
	line := func(s string) string {
		for _, l := range strings.Split(s, "\n") {
			if strings.HasPrefix(l, "Head and shoulders,") {
				return l
			}
		}
		return ""
	}
	if line(bare) == "" || line(bare) != line(withDetail) {
		t.Errorf("кадр сдвинулся от приметы:\n  %s\n  %s", line(bare), line(withDetail))
	}
}
