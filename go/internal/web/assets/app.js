// Единственный скрипт площадки, и он ничего не создаёт — только сворачивает
// ветки в дереве, как «Ответы N» на НГС. Всё остальное страница умеет без
// него: это условие, а не стиль, потому что строгий CSP запрещает
// inline-скрипты, а внешних зависимостей (npm, CDN) у площадки нет.
//
// Дерево приходит ЦЕЛИКОМ, плоским списком с отступами, поэтому потомки
// комментария — это идущие следом элементы с большей глубиной. Число ответов
// заново не считается — оно уже стоит в «Ответы N» с сервера; считается только
// ДЛИНА ветки, чтобы знать, докуда она тянется.
//
// Вид пересчитывается ЗАНОВО из состояния всех кнопок (apply), а не
// переключается на месте у своих потомков: свёрнутая ветка внутри свёрнутой —
// обычное дело, и разворот верхней вытаскивал бы наружу то, что свёрнуто
// внутри неё. У НГС этой заботы нет вовсе: там ветка ПЛОСКАЯ — одна кнопка на
// корневой комментарий и `$('.js-comment__replies-<id>').toggle()` на всех его
// потомков сразу, — а у нас кнопка есть на каждом уровне.
(function () {
  'use strict';

  var list = document.querySelector('.thread:not(.linear)');
  if (!list) return;

  var items = Array.prototype.slice.call(list.querySelectorAll('.c'));
  var depth = function (el) { return parseInt(el.getAttribute('data-depth'), 10) || 1; };
  var folds = [];   // кнопки ветвей в порядке страницы
  var byItem = {};  // номер строки → её кнопка
  var all = null;   // «Свернуть все ответы», если заголовок треда нашёлся

  items.forEach(function (el, i) {
    var box = el.querySelector('.rcount');
    if (!box) return; // ветки нет — сворачивать нечего

    var d = depth(el), n = 0;
    for (var j = i + 1; j < items.length && depth(items[j]) > d; j++) n++;
    if (n === 0) return;

    var btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'fold';
    var f = { btn: btn, label: box.textContent, off: false };
    btn.addEventListener('click', function () {
      f.off = !f.off;
      apply();
    });
    box.replaceWith(btn);
    byItem[i] = f;
    folds.push(f);
  });
  if (folds.length === 0) return;

  // Строка скрыта, если свёрнут хоть один её предок. cut — глубина ближайшего
  // свёрнутого предка; его ветка кончается на первой строке не глубже него.
  function apply() {
    var cut = 0;
    items.forEach(function (el, i) {
      var d = depth(el);
      if (cut && d <= cut) cut = 0;
      var hide = cut > 0;
      el.hidden = hide;
      var f = byItem[i];
      if (f && !hide && f.off) cut = d; // сама строка видна, потомки — нет
    });
    folds.forEach(function (f) {
      f.btn.setAttribute('aria-expanded', f.off ? 'false' : 'true');
      f.btn.textContent = f.off ? 'Показать ' + f.label.toLowerCase() : f.label;
    });
    if (!all) return;
    // Подпись общей кнопки выведена из состояния ветвей, а не хранится своей:
    // иначе, свернув всё и раскрыв одну ветку руками, человек читал бы на ней
    // «Развернуть все ответы» и нажимал впустую.
    var open = folds.some(function (f) { return !f.off; });
    all.setAttribute('aria-expanded', open ? 'true' : 'false');
    all.textContent = open ? 'Свернуть все ответы' : 'Развернуть все ответы';
  }

  // «Свернуть все ответы» — НАША кнопка, справа от заголовка треда; оригинал
  // такой не знает, потому что у него дерево и открывается свёрнутым (ветки
  // приходят с `lv-hidden` в разметке: на живой странице 256 строк из 325).
  // Мы отдаём дерево развёрнутым — иначе без JS ответов не видно вовсе, — и
  // тогда обратное действие нужно одним нажатием. Ставит её скрипт, а не
  // шаблон: без JS сворачивать нечем, а мёртвая ссылка хуже её отсутствия.
  var head = document.querySelector('.cttl');
  if (head) {
    all = document.createElement('button');
    all.type = 'button';
    all.className = 'fold foldall';
    all.addEventListener('click', function () {
      var off = folds.some(function (f) { return !f.off; }); // есть что сворачивать
      folds.forEach(function (f) { f.off = off; });
      apply();
    });
    head.appendChild(all);
  }
  apply();
})();

// Меню участника закрывается по клику мимо и по Escape. Открывает и закрывает
// его сам браузер (details/summary) — здесь только то, чего разметка не умеет.
// Поэтому и отдельной функцией: сворачивание веток выше выходит раньше, если
// дерева на странице нет, а меню есть на каждой.
(function () {
  'use strict';

  var menu = document.querySelector('details.acct');
  if (!menu) return; // гость: меню нет, есть кнопка «Вход»

  document.addEventListener('click', function (e) {
    if (menu.open && !menu.contains(e.target)) menu.open = false;
  });
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape' && menu.open) {
      menu.open = false;
      var btn = menu.querySelector('summary');
      if (btn) btn.focus();
    }
  });
})();

// Выбиралка смайлов: панель под формой становится кликабельной.
//
// Сервер рисует её СПРАВОЧНИКОМ — картинка и код рядом, — потому что без
// скрипта код всё равно набирается руками, как на НГС все эти годы. Здесь
// справочник превращается в кнопки, вставляющие код на месте курсора: то же
// правило, что у сворачивания веток — мёртвых кнопок на странице не бывает.
//
// Функция, а не замкнутый в себе кусок, потому что оживлять панель приходится
// ДВАЖДЫ: при загрузке страницы и ещё раз у формы ответа, которую скрипт
// приносит с сервера уже после (ниже). Повторный вызов безопасен: отбор идёт по
// [data-code], а у оживлённой кнопки этого атрибута нет.
function smilePanel(scope) {
  'use strict';

  var boxes = scope.querySelectorAll('details.smbox');
  if (!boxes.length) return;

  Array.prototype.forEach.call(boxes, function (box) {
    var form = box.closest('form');
    var area = form && form.querySelector('textarea');
    if (!area) return;

    Array.prototype.forEach.call(box.querySelectorAll('.smi[data-code]'), function (item) {
      var code = ':::' + item.getAttribute('data-code') + ':::';
      var btn = document.createElement('button');
      btn.type = 'button'; // не submit: панель стоит ВНУТРИ формы
      btn.className = 'smi';
      btn.title = code;
      btn.appendChild(item.firstChild.cloneNode(true)); // картинка без кода
      btn.addEventListener('click', function () {
        var at = area.selectionStart, to = area.selectionEnd;
        // Пробел перед кодом, если человек не оставил его сам: на сайте смайл
        // отделён от слова, а слипшийся «спасибо:::flowers:::» читается хуже.
        var before = area.value.slice(0, at);
        var pad = before === '' || /\s$/.test(before) ? '' : ' ';
        area.value = before + pad + code + ' ' + area.value.slice(to);
        area.focus();
        var pos = at + pad.length + code.length + 1;
        area.setSelectionRange(pos, pos);
      });
      item.replaceWith(btn);
    });
  });
}
smilePanel(document);

// Форма ответа открывается НА МЕСТЕ, без перезагрузки.
//
// До этого «Ответить» было обычной ссылкой на ?reply=<id>, и стоило это полной
// перерисовки треда — до 5000 строк ради одной формы, — да ещё и потери места, на
// котором человек читал. Ссылкой оно и остаётся: без скрипта дорога прежняя, и
// это условие, а не любезность.
//
// Разметку скрипт НЕ СОБИРАЕТ. Он просит у сервера готовую строку (replyform.go),
// которую рисует тот же шаблон, что и страницу, — ровно как живой добор ниже.
// Своей сборкой он не отличил бы участника от тени, а метка «ещё не переехал
// сюда с НГС» отвечает на единственный вопрос отвечающего: дойдёт ли ответ.
(function () {
  'use strict';

  var thread = document.querySelector('.thread');
  var note = location.pathname.match(/^\/n\/(\d+)$/);
  if (!thread || !note || !window.fetch) return;

  var busy = false;

  // Нижняя форма — та, что не лежит строкой треда: она про ответ НА ЗАМЕТКУ.
  // Ищется на каждое нажатие, потому что строка с формой приходит и уходит.
  var bottomBox = function () {
    var found = null;
    Array.prototype.forEach.call(document.querySelectorAll('.replybox'), function (b) {
      if (!b.closest('.replyrow')) found = b;
    });
    return found;
  };

  // Набранное переносится в новую форму: человек начал писать одному, передумал
  // и нажал «Ответить» другому — терять его слова из-за этого не за что.
  var carry = function (row) {
    var was = thread.querySelector('.replyrow textarea');
    var now = row.querySelector('textarea');
    if (was && now && was.value) now.value = was.value;
  };

  var drop = function () {
    var row = thread.querySelector('.replyrow');
    if (row) row.parentNode.removeChild(row);
  };

  // Адрес страницы меняется под то, что на ней открыто: обновление вернёт форму
  // на то же место, то есть обе дороги — со скриптом и без — сходятся в одном
  // адресе. Прокрутки при этом нет: replaceState якорь не отрабатывает.
  var remember = function (href) {
    if (window.history && history.replaceState) history.replaceState(null, '', href);
  };

  var open = function (link, cid) {
    if (busy) return;
    busy = true;
    // Вид треда и номер страницы уже стоят в самой ссылке — «Ответить» не должно
    // уводить человека из линейного вида в дерево. Отсюда и подмена reply на to
    // вместо сборки адреса заново.
    var url = '/n/' + note[1] + '/reply' + link.search.replace(/([?&])reply=/, '$1to=');
    fetch(url, { credentials: 'same-origin', headers: { 'Accept': 'text/html' } })
      .then(function (res) {
        if (!res.ok) throw new Error('replyform');
        return res.text();
      })
      .then(function (html) {
        var box = document.createElement('template');
        box.innerHTML = html;
        var row = box.content.firstElementChild;
        var at = document.getElementById('c' + cid);
        if (!row || !at) throw new Error('replyform');
        carry(row);
        drop();
        at.parentNode.insertBefore(row, at.nextSibling);
        // Якорь #reply остаётся у НИЖНЕЙ формы: она тут одна такая на странице,
        // и ведёт на неё «в общий тред». Строка же приходит с сервера в том
        // виде, в каком он рисует её ЕДИНСТВЕННОЙ формой страницы (без скрипта
        // нижней рядом не бывает), — здесь их две, а один и тот же id у двух
        // элементов делает адрес #reply бессмысленным.
        var boxed = row.querySelector('.replybox');
        if (boxed && bottomBox()) boxed.removeAttribute('id');
        smilePanel(row);
        var area = row.querySelector('textarea');
        if (area) area.focus();
        remember(link.href);
      })
      .catch(function () {
        // Реплику снесли, сессия истекла, сеть отпала — во всех случаях честнее
        // уйти по той же ссылке обычным переходом: страница покажет, как есть.
        location.href = link.href;
      })
      .then(function () { busy = false; });
  };

  document.addEventListener('click', function (e) {
    if (e.button || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    if (!e.target.closest) return;

    var link = e.target.closest('a.rep');
    if (link) {
      var li = link.closest('li.c');
      if (!li || !li.id) return;
      e.preventDefault();
      open(link, li.id.slice(1));
      return;
    }

    // «в общий тред» — обратное действие, и перезагружать ради него страницу
    // так же незачем. Работает это, только пока нижняя форма на месте: на
    // странице, открытой по ?reply=<id>, её нет вовсе (её рисует сервер вместо
    // строки в треде), и там ссылка остаётся ссылкой.
    var alt = e.target.closest('.replyrow .repto .alt');
    if (!alt) return;
    var bottom = bottomBox();
    if (!bottom) return;
    e.preventDefault();
    drop();
    remember(alt.href);
    var area = bottom.querySelector('textarea');
    if (area) area.focus();
  });
})();

// Живая страница: тред и лента дописывают себя сами, без перезагрузки и без
// нажатий.
//
// Работает это в два шага, и разделение принципиальное (подробности — в шапке
// fresh.go). Поток /live шлёт СИГНАЛ: «в этом треде новое». Разметку он не
// несёт и нести не должен — хаб один на процесс и никого не знает, а строка у
// каждого своя: модератор видит скрытое, автор свою реакцию, гость не видит
// «Ответить». Получив сигнал, страница идёт за готовой строкой на /fresh, где
// её рисует ТОТ ЖЕ шаблон, что и саму страницу.
//
// Поэтому здесь нет ни сборки разметки, ни экранирования: скрипт только
// вставляет полученный кусок и решает, КУДА он встанет. Разбор текста в HTML
// остаётся ровно в одном месте — на сервере.
//
// Без скрипта не появляется ничего, и это не ущерб: страница целиком работает
// обновлением, а живой добор — удобство поверх неё. Поток открывается только у
// вошедшего — признаком служит колокольчик в шапке, которого у гостя нет.
(function () {
  'use strict';

  var bell = document.querySelector('.bell');
  if (!bell || !window.EventSource || !window.fetch) return;

  // Что слушаем. Тред узнаём из адреса, а не из разметки: /n/312811 — это и
  // есть номер заметки, и лишний атрибут в шаблоне ради него не нужен.
  var m = location.pathname.match(/^\/n\/(\d+)$/);
  var query = m ? '?note=' + m[1] : (location.pathname === '/' ? '?feed=1' : '');
  if (!m && query === '') return; // на остальных страницах слушать нечего

  // Список, который дописывается, и граница добора. Атрибут data-fresh и есть
  // выключатель: его нет на страницах линейного вида кроме первой — там срез
  // истории, и дописывать в него хвост разговора значит врать о том, что
  // человек читает. Сигналы при этом принимаются всё равно: колокольчик живёт
  // на любой странице.
  var list = document.querySelector(m ? '.thread' : '.notes');
  var url = list && list.getAttribute('data-fresh-url');
  var cursor = list && list.getAttribute('data-fresh');
  var linear = !!list && list.classList.contains('linear');
  // «Проматывать к новым» — настройка человека с /me (jump.go). Атрибут стоит
  // внутри того же if, что и граница добора: прыгать некуда там, где ничего не
  // дописывается.
  var jump = !!list && list.getAttribute('data-jump') === '1';
  var src = null, busy = false, again = false, timer = null;

  // Счётчик у колокольчика подкручивается на месте, а не перечитывается с
  // сервера: лишний запрос ради одной цифры дороже самой цифры, а точное
  // значение приедет со следующей же страницей.
  var poke = function () {
    var cnt = bell.querySelector('.cnt');
    if (!cnt) {
      cnt = document.createElement('span');
      cnt.className = 'cnt';
      cnt.textContent = '0';
      bell.appendChild(cnt);
      bell.classList.add('has');
    }
    var n = parseInt(cnt.textContent, 10);
    cnt.textContent = isNaN(n) ? '1' : (n >= 99 ? '99+' : String(n + 1));
  };

  // Номер реплики — из её же id в разметке. Сравнивать обязательно ЧИСЛАМИ:
  // «100000000719» и «63238683» как строки идут не в том порядке.
  var num = function (el) {
    return el && el.id && el.id.charAt(0) === 'c' ? parseInt(el.id.slice(1), 10) : 0;
  };

  // Куда встанет новая реплика в ДЕРЕВЕ. Место то же, что дало бы обновление, и
  // это не догадка о порядке: страница идёт по материализованному пути, а
  // сегмент пути — это id, дополненный нулями до общей ширины, то есть среди
  // СЕСТЁР порядок по возрастанию id.
  //
  // Раньше строка дописывалась в конец ветки — «свой id самый большой». На
  // смешанном треде это неверно: номера своих и зеркальных реплик из разных
  // полос, нативный больше любого ngs'ного, поэтому пришедший с НГС ответ
  // по-настоящему стоит ВЫШЕ уже лежащих своих, и обновление страницы его туда и
  // переставляло.
  //
  // Ветка адресата кончается на первой строке не глубже него, сёстры — строки
  // ровно на уровень глубже. Ответ в корень треда (data-parent нет вовсе) ищет
  // сестёр с начала списка; адресат, которого на странице нет (скрыт), оставляет
  // строку в конце — угадывать за него незачем.
  var placeInTree = function (li) {
    var parent = li.getAttribute('data-parent');
    var anchor = null;
    if (parent) {
      anchor = document.getElementById('c' + parent);
      if (!anchor) { list.appendChild(li); return; }
    }
    var depth = anchor ? parseInt(anchor.getAttribute('data-depth'), 10) : 0;
    var id = num(li);
    var at = anchor ? anchor.nextElementSibling : list.firstElementChild;
    while (at) {
      // Не всякая строка треда — реплика: под адресатом может стоять форма
      // ответа. Веткой она не является и конца её не означает, поэтому её просто
      // проходим.
      if (at.classList.contains('c')) {
        var d = parseInt(at.getAttribute('data-depth'), 10);
        if (d <= depth) { break; }                       // ветка адресата кончилась
        if (d === depth + 1 && num(at) > id) { break; }  // нашлась младшая сестра
      }
      at = at.nextElementSibling;
    }
    list.insertBefore(li, at); // at === null → в самый конец
  };

  // В ЛЕНТЕ новое идёт наверх, но НИЖЕ закреплённого: закреплённые стоят вне
  // хронологии, и свежая заметка над ними выглядела бы поломкой сортировки.
  var placeOnTop = function (li) {
    var at = list.firstElementChild;
    while (at && at.className.indexOf('pinned') >= 0) at = at.nextElementSibling;
    list.insertBefore(li, at);
  };

  // Какие строки порции — ПЕРЕЕЗДЫ (заголовок X-Fresh-Moved). Дерево под
  // открытой страницей перестраивается: зеркало ставит ребро по обращению
  // «Ник, …», а обход мобильной версии заменяет его настоящим — и ветка,
  // нарисованная по догадке, уезжает на своё место. Такая строка приходит второй
  // раз, ГОТОВОЙ разметкой: вместе с местом у неё сменились глубина, подпись
  // адресата, а то и тело.
  var movedSet = function (h) {
    var s = {};
    (h || '').split(',').forEach(function (v) { if (v) s['c' + v] = true; });
    return s;
  };

  // Возвращает ПЕРВУЮ по порядку страницы из дописанных строк — ту, на которую
  // встаёт «Проматывать к новым».
  //
  // Именно первую, а не последнюю, хотя просьба владельца звучала как «самый
  // свежий комментарий». В ЛИНЕЙНОМ виде это одно и то же: новое кладётся
  // сверху, и первое по странице оно же и самое свежее. В ДЕРЕВЕ «самый свежий»
  // из пришедшей пачки неопределим здесь вовсе — порядок веток не порядок
  // времени, а сравнивать по номеру нельзя: полосы идентификаторов не
  // упорядочиваются между собой, нативная реплика позапрошлой недели имеет
  // номер больше пришедшей с НГС сегодня. Первая же по странице — единственный
  // выбор, после которого НИ ОДНА новая строка не осталась ЗА СПИНОЙ: дальше
  // они идут вниз, туда, куда и читают.
  // Строку, которую человек, возможно, ЧИТАЕТ ПРЯМО СЕЙЧАС, переезд снимает со
  // страницы и ставит в другую ветку — и до 27.08.2026 она просто исчезала
  // из-под глаз: подсветки переезду не ставят (строка не новая), целью прыжка он
  // не считается, — искать её заново приходилось глазами по всему треду.
  //
  // Чинится это не «переводом фокуса»: прокрутка к новому месту — тот же рывок
  // страницы, от которого бережёт handsBusy. Сохраняется МЕСТО ЧТЕНИЯ — высота,
  // на которой строка стояла на экране: после перестановки она туда и
  // возвращается. Двигается окружение, а не читатель.
  var visibleTop = function (el) {
    var r = el.getBoundingClientRect();
    var h = window.innerHeight || document.documentElement.clientHeight;
    return (r.bottom > 0 && r.top < h) ? r.top : null;
  };

  // Вернуть строку на прежнюю высоту. Считается ПОСЛЕ всей порции: строка,
  // вставленная выше, сдвинула бы её ещё раз. Мгновенно и без анимации — это не
  // переход к новому, а отмена чужого движения; сдвиг меньше пикселя не трогаем
  // вовсе, его уже разобрал сам браузер (overflow-anchor).
  var keepPlace = function (el, top) {
    if (!el) return;
    var d = el.getBoundingClientRect().top - top;
    if (Math.abs(d) > 1) window.scrollBy(0, d);
  };

  // Занята ли ЭТА строка. handsBusy() спрашивает про страницу целиком и сторожит
  // прокрутку; вопрос здесь другой — не отнимаем ли мы работу, снимая узел со
  // страницы: выделение и курсор живут В УЗЛЕ и умирают вместе с ним.
  // Список полей тот же, что у handsBusy, и это не совпадение: держать строку
  // из-за нажатой внутри неё ссылки нельзя — фокус с неё может не уйти вовсе, и
  // переезд не случился бы никогда.
  var busyIn = function (el) {
    var a = document.activeElement;
    if (a && el.contains(a) &&
        (a.tagName === 'TEXTAREA' || a.tagName === 'INPUT' || a.isContentEditable)) return true;
    var sel = window.getSelection && window.getSelection();
    if (!sel || sel.isCollapsed || !String(sel).replace(/\s+/g, '')) return false;
    return el.contains(sel.anchorNode) || el.contains(sel.focusNode);
  };

  // Отложенные переезды. Держим их у себя, потому что второй раз они не придут:
  // курсор добора ушёл вперёд, а отметка moved_at прочитана.
  var held = {};

  // Перестановка одной строки. Возвращает высоту, на которой она стояла (null —
  // её не было видно, и место чтения не про неё).
  var relocate = function (li, old) {
    var top = visibleTop(old);
    old.parentNode.removeChild(old);
    placeInTree(li);
    // Метка переезда — не подсветка нового: текст прежний, и отвечает она на
    // другой вопрос, «почему вокруг всё поменялось». Живёт в CSS, гаснет сама.
    li.className += ' moved';
    return top;
  };

  // Порция переездов применяется ЦЕЛИКОМ или не применяется вовсе. Дерево —
  // связная структура (тем же доводом ApplyReplyTree на сервере кладёт заметку
  // одной транзакцией): переставив родителя и оставив на месте потомка, страница
  // нарисует ветку глубиной МЕНЬШЕ родительской — ровно ту картинку, из-за
  // которой переезды и заведены.
  var applyMoves = function (moves) {
    var i, k;
    for (i = 0; i < moves.length; i++) {
      // Хоть одну строку держат — ждём все: отпустят, и flushHeld переставит.
      if (busyIn(moves[i][1])) {
        for (k = 0; k < moves.length; k++) held[moves[k][0].id] = moves[k][0];
        return;
      }
    }
    var anchor = null, top = 0;
    for (i = 0; i < moves.length; i++) {
      var li = moves[i][0], old = moves[i][1];
      // В линейном виде и в ленте место не менялось (там порядок по времени), и
      // строка просто заменяет себя на месте.
      if (!m || linear) { old.replaceWith(li); continue; }
      var was = relocate(li, old);
      if (was !== null && anchor === null) { anchor = li; top = was; }
    }
    keepPlace(anchor, top);
  };

  // Отпустили — переезжаем. selectionchange ловит и снятие выделения, и клик
  // мимо, focusout — уход курсора из формы; пустой набор стоит одной проверки.
  var flushHeld = function () {
    var ids = Object.keys(held), moves = [];
    if (!ids.length) return;
    for (var i = 0; i < ids.length; i++) {
      var li = held[ids[i]], old = document.getElementById(li.id);
      delete held[ids[i]];
      if (old) moves.push([li, old]); // строку могло унести обновлением страницы
    }
    applyMoves(moves); // всё ещё держат — вернётся в held
  };

  var insert = function (html, moved) {
    if (!html) return null;
    var box = document.createElement('template');
    box.innerHTML = html;
    var items = Array.prototype.slice.call(box.content.children);
    var fresh = [], moves = [], first = null, i;
    for (i = 0; i < items.length; i++) {
      var li = items[i];
      if (!li.id) continue;
      var old = document.getElementById(li.id);
      if (old) {
        // Своя же только что отправленная реплика уже нарисована страницей, а
        // сигнал о ней придёт всё равно: поток не знает, кто автор. Проверка по
        // id закрывает это разом — и заодно любой повтор добора.
        if (!moved[li.id]) continue;
        // Переезд. В дереве строку надо ПЕРЕСТАВИТЬ, поэтому старую снимаем и
        // ищем ей место заново; потомки приедут той же порцией и встанут следом
        // — переезд ветки родитель открывает, у него меньший id.
        moves.push([li, old]);
        continue;
      }
      // Переезд строки, которой на странице нет вовсе (линейный вид показывает
      // окно, у дерева есть потолок): ставить её некуда, а «сверху» в линейном
      // виде значило бы поднять старую реплику наверх. Если она и правда новая,
      // её принесёт обычный добор — граница по id её ещё не прошла.
      if (moved[li.id]) continue;
      fresh.push(li);
    }
    // Переезды ПЕРВЫМИ: новая реплика приезжает с глубиной, посчитанной уже
    // после перестановки, и встать ей надо к родителю на новом месте.
    applyMoves(moves);
    for (i = 0; i < fresh.length; i++) {
      var f = fresh[i];
      if (!m) { placeOnTop(f); }
      else if (linear) { list.insertBefore(f, list.firstChild); }
      else { placeInTree(f); }
      // Подсветка — единственное, чем новое отличается от старого, и живёт она
      // в CSS: класс ставится навсегда, а гаснет анимацией. Снимать его таймером
      // незачем, свою работу он к тому времени уже сделал. У переезда метка своя
      // (relocate): строка не новая, и вопрос у читателя другой.
      f.className += ' fresh';
      // Порядок страницы, а не порядок пачки: в дереве строка садится в свою
      // ветку, то есть куда угодно, в том числе выше уже вставленной.
      if (!first || (f.compareDocumentPosition(first) & 4)) first = f;
    }
    if (first) {
      var empty = document.querySelector('.empty');
      if (empty && empty.parentNode) empty.parentNode.removeChild(empty);
    }
    return first;
  };

  // Страница не прыгает из-под рук. Два случая, и оба узнаются дёшево: человек
  // НАБИРАЕТ (курсор в поле — форма ответа стоит прямо в треде) и человек
  // ВЫДЕЛИЛ текст (читает или копирует). Прокрутка в эти секунды — это потеря
  // работы и потеря места, а новая реплика подсвечена и никуда не денется.
  var handsBusy = function () {
    var a = document.activeElement;
    if (a && (a.tagName === 'TEXTAREA' || a.tagName === 'INPUT' || a.isContentEditable)) return true;
    var sel = window.getSelection && window.getSelection();
    return !!(sel && !sel.isCollapsed && String(sel).replace(/\s+/g, ''));
  };

  // Плавно — но не тому, кто попросил систему не двигать картинку: для него
  // прокрутка мгновенная. Отступ под липкую шапку приезжает даром — он стоит
  // у самой реплики (scroll-margin-top в style.css), и scrollIntoView его
  // соблюдает, иначе строка встала бы ПОД шапкой.
  var jumpTo = function (el) {
    if (!jump || !el || handsBusy()) return;
    var still = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    try { el.scrollIntoView({ block: 'start', behavior: still ? 'auto' : 'smooth' }); }
    catch (e) { el.scrollIntoView(true); }
  };

  // Число над тредом ставит СЕРВЕР, а не счёт вставленных строк: порция добора
  // ограничена потолком, да и скрытая реплика в неё не попадёт вовсе.
  var setCount = function (n) {
    if (!n) return;
    // Число стоит в двух местах сразу: над тредом и в липкой шапке. Оба
    // подкручиваются на месте — перечитывать страницу ради одной цифры дороже
    // самой цифры.
    var head = document.querySelector('.cttl .cnt');
    if (head) head.textContent = n;
    var top = document.querySelector('.hcount .n');
    if (top) top.textContent = n;
  };

  var pull = function () {
    if (!url || cursor === null || cursor === undefined) return;
    if (busy) { again = true; return; }
    busy = true;
    var next = null, count = null, moved = null;
    fetch(url + '?after=' + encodeURIComponent(cursor) + (linear ? '&view=linear' : ''),
      { credentials: 'same-origin', headers: { 'Accept': 'text/html' } })
      .then(function (res) {
        if (!res.ok) throw new Error('fresh');
        next = res.headers.get('X-Fresh-After');
        count = res.headers.get('X-Fresh-Count');
        moved = movedSet(res.headers.get('X-Fresh-Moved'));
        return res.text();
      })
      .then(function (html) {
        if (next) cursor = next;
        jumpTo(insert(html, moved));
        setCount(count);
      })
      .catch(function () {
        // Отказ добора — не поломка страницы: следующий сигнал попробует снова,
        // а до тех пор человек видит ровно то, что видел.
      })
      .then(function () {
        busy = false;
        if (again) { again = false; schedule(); }
      });
  };

  // Разброс обязателен. Сигнал о новой реплике приходит ВСЕМ, кто держит тред
  // открытым, в одну и ту же секунду, — и без него они пошли бы за добором
  // разом. У морды двенадцать слотов и четыре соединения к Postgres, так что
  // популярный тред устроил бы отказ сам себе.
  //
  // Окно — пятая часть прежнего, и это плата за мгновенность: сигнал теперь
  // приходит звонком из базы, а не по такту в две секунды, и секунда разброса
  // поверх него съедала бы ровно то, что было выиграно. Арифметика на худший
  // случай сходится: живых потоков у площадки 64 ВСЕГО (потолок в live.go),
  // добор — один запрос по индексу, десяток-другой миллисекунд, то есть при
  // окне в 200 мс одновременно в работе оказывается единицы запросов из
  // двенадцати слотов. Растить потолок потоков, не пересчитав это число,
  // нельзя.
  //
  // Второе назначение окна — склейка: пока добор не ушёл, все пришедшие звонки
  // становятся одним запросом (timer занят), а звонки во время самого запроса
  // копятся в again и дают ровно один повтор.
  var schedule = function () {
    if (timer) return;
    timer = window.setTimeout(function () { timer = null; pull(); },
      Math.floor(Math.random() * 200));
  };

  var open = function () {
    if (src) return;
    src = new EventSource('/live' + query);
    src.onmessage = function (e) {
      var d;
      try { d = JSON.parse(e.data); } catch (err) { return; }
      if (d.kind === 'poke') { poke(); return; }
      schedule();
    };
    // Переподключение EventSource берёт на себя сам; наше дело — не мешать.
    // Свой поток сервер закрывает каждые пять минут, и это штатный путь.
  };

  var close = function () {
    if (!src) return;
    src.close();
    src = null;
  };

  // Вкладка в фоне слот не занимает: соединений у площадки шестьдесят четыре
  // на всех, и держать их за свёрнутыми окнами незачем. Возвращаясь, страница
  // добирает пропущенное сразу: сигналов за время сна она не слышала.
  // Отложенный переезд ждёт, пока строку отпустят: выделение снято, курсор ушёл.
  document.addEventListener('selectionchange', flushHeld);
  document.addEventListener('focusout', flushHeld);

  document.addEventListener('visibilitychange', function () {
    if (document.hidden) { close(); } else { open(); schedule(); }
  });
  if (!document.hidden) open();
})();

// Картинка уходит на сервер СРАЗУ по выбору, а не вместе с заметкой.
//
// Что это чинит. Раньше файл ехал телом той же формы: человек писал заметку,
// нажимал «Опубликовать» и минуту смотрел на молчащий экран (десять мегабайт с
// телефона столько и ползут), а отказ — «это не картинка», «слишком много
// точек» — приходил в самом конце, вместе с потерей выбранного файла: обратно
// браузер его не отдаёт. Теперь закачка идёт, пока пишется текст, отказ виден
// сразу, а форма отправляется с одним номером черновика (shotdraft.go).
//
// Кнопка отправки на время закачки неактивна — публиковать всё равно нечего,
// пока картинка не доехала, а нажатие в этот момент отправило бы заметку без
// неё.
//
// Разметку скрипт НЕ СОБИРАЕТ и здесь: карточку с превью и скрытым полем рисует
// сервер (parts/shotdraft.gohtml), тот же шаблон, что ставит её на форму после
// отказа публикации. Скрипт лишь кладёт присланное в готовый короб.
//
// Отказ этой дороги НЕ ломает прежнюю: если предзагрузка не удалась по сетевой
// причине, выбор файла остаётся в поле, и он уедет телом формы, как раньше.
(function () {
  'use strict';

  var form = document.querySelector('form.wform');
  if (!form || !window.FormData || !window.XMLHttpRequest) return;

  var input = form.querySelector('input[type=file][name=shot]');
  var box = form.querySelector('.shotbox');
  var wait = form.querySelector('.shotwait');
  var bar = form.querySelector('.shotbar');
  var send = form.querySelector('button[type=submit]');
  if (!input || !box || !wait || !bar || !send) return;

  var busy = false;

  var idle = function () {
    busy = false;
    wait.hidden = true;
    send.disabled = false;
  };

  // Отправлять, пока картинка едет, нечего: Enter в поле текста и второе
  // нажатие кнопки — те же самые «опубликовать без неё».
  form.addEventListener('submit', function (e) {
    if (busy) e.preventDefault();
  });

  // «Убрать»: карточка уходит вместе со скрытым полем, и заметка публикуется
  // без картинки. Сам черновик на сервере не трогаем — он вытеснится следующим
  // выбором или протухнет сам; отдельный запрос ради этого не стоит того.
  form.addEventListener('click', function (e) {
    if (!e.target.closest) return;
    if (e.target.closest('.shotdrop')) {
      box.innerHTML = '';
      input.value = '';
    }
  });

  input.addEventListener('change', function () {
    if (busy || !input.files || !input.files.length) return;
    busy = true;
    box.innerHTML = '';
    bar.value = 0;
    wait.hidden = false;
    send.disabled = true;

    var data = new FormData();
    // CSRF первым полем — ровно как в разметке формы: порядок частей multipart
    // это порядок в разметке, и токен обязан быть проверен до чтения файла.
    var csrf = form.querySelector('input[name=csrf]');
    if (csrf) data.append('csrf', csrf.value);
    data.append('shot', input.files[0]);

    var xhr = new XMLHttpRequest();
    xhr.open('POST', '/shot');
    xhr.setRequestHeader('Accept', 'text/html');
    // Полоса — весь видимый смысл затеи: «загружается» без числа не отличается
    // от «зависло», а именно им это раньше и выглядело.
    xhr.upload.addEventListener('progress', function (e) {
      if (e.lengthComputable) bar.value = Math.round((e.loaded / e.total) * 100);
    });
    xhr.addEventListener('load', function () {
      // 200 — карточка с превью, 400 — отказ по самому файлу. И то и другое
      // сервер прислал готовой разметкой, и в обоих случаях выбор из поля
      // снимается: файл либо уже на сервере, либо негоден, и второй раз ехать
      // ему незачем.
      if (xhr.status === 200 || xhr.status === 400) {
        box.innerHTML = xhr.responseText;
        input.value = '';
      }
      // Прочее (сессия истекла, площадка занята, потолок частоты) — не про
      // файл, а про нас: оставляем выбор в поле, чтобы он уехал обычной
      // дорогой вместе с заметкой.
      idle();
    });
    xhr.addEventListener('error', idle);
    xhr.addEventListener('abort', idle);
    xhr.addEventListener('timeout', idle);
    xhr.send(data);
  });
})();
