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
(function () {
  'use strict';

  var boxes = document.querySelectorAll('details.smbox');
  if (!boxes.length) return;

  Array.prototype.forEach.call(boxes, function (box) {
    var form = box.closest('form');
    var area = form && form.querySelector('textarea');
    if (!area) return;

    Array.prototype.forEach.call(box.querySelectorAll('.smi'), function (item) {
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
})();
