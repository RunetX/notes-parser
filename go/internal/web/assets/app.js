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

  // Куда встанет новая реплика в ДЕРЕВЕ: после всей ветки того, кому отвечали.
  // Это не догадка о порядке, а он самый: путь новой реплики считается от её
  // же id, поэтому среди соседей по ветке он последний. Родителя на странице
  // нет (ответ в корень треда) — значит в конец.
  var placeInTree = function (li) {
    var parent = li.getAttribute('data-parent');
    var anchor = parent ? document.getElementById('c' + parent) : null;
    if (!anchor) { list.appendChild(li); return; }
    var depth = parseInt(anchor.getAttribute('data-depth'), 10);
    var at = anchor.nextElementSibling;
    // Не всякая строка треда — реплика: под адресатом может стоять форма
    // ответа. Веткой она не является и конца её не означает, поэтому её просто
    // проходим — иначе новая реплика встала бы ВЫШЕ уже лежащих соседей.
    while (at && (!at.classList.contains('c') ||
                  parseInt(at.getAttribute('data-depth'), 10) > depth)) {
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

  var insert = function (html) {
    if (!html) return 0;
    var box = document.createElement('template');
    box.innerHTML = html;
    var items = Array.prototype.slice.call(box.content.children), added = 0;
    for (var i = 0; i < items.length; i++) {
      var li = items[i];
      // Своя же только что отправленная реплика уже нарисована страницей, а
      // сигнал о ней придёт всё равно: поток не знает, кто автор. Проверка по
      // id закрывает это разом — и заодно любой повтор добора.
      if (!li.id || document.getElementById(li.id)) continue;
      if (!m) { placeOnTop(li); }
      else if (linear) { list.insertBefore(li, list.firstChild); }
      else { placeInTree(li); }
      // Подсветка — единственное, чем новое отличается от старого, и живёт она
      // в CSS: класс ставится навсегда, а гаснет анимацией. Снимать его
      // таймером незачем, свою работу он к тому времени уже сделал.
      li.className += ' fresh';
      added++;
    }
    if (added) {
      var empty = document.querySelector('.empty');
      if (empty && empty.parentNode) empty.parentNode.removeChild(empty);
    }
    return added;
  };

  // Число над тредом ставит СЕРВЕР, а не счёт вставленных строк: порция добора
  // ограничена потолком, да и скрытая реплика в неё не попадёт вовсе.
  var setCount = function (n) {
    var head = document.querySelector('.cttl .cnt');
    if (head && n) head.textContent = n;
  };

  var pull = function () {
    if (!url || cursor === null || cursor === undefined) return;
    if (busy) { again = true; return; }
    busy = true;
    var next = null, count = null;
    fetch(url + '?after=' + encodeURIComponent(cursor) + (linear ? '&view=linear' : ''),
      { credentials: 'same-origin', headers: { 'Accept': 'text/html' } })
      .then(function (res) {
        if (!res.ok) throw new Error('fresh');
        next = res.headers.get('X-Fresh-After');
        count = res.headers.get('X-Fresh-Count');
        return res.text();
      })
      .then(function (html) {
        if (next) cursor = next;
        insert(html);
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
  var schedule = function () {
    if (timer) return;
    timer = window.setTimeout(function () { timer = null; pull(); },
      Math.floor(Math.random() * 1200));
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
  document.addEventListener('visibilitychange', function () {
    if (document.hidden) { close(); } else { open(); schedule(); }
  });
  if (!document.hidden) open();
})();
