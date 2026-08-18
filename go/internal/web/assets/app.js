// Единственный скрипт площадки, и он ничего не создаёт — только сворачивает
// ветки в дереве, как «Ответы N» на НГС. Всё остальное страница умеет без
// него: это условие, а не стиль, потому что строгий CSP запрещает
// inline-скрипты, а внешних зависимостей (npm, CDN) у площадки нет.
//
// Дерево приходит ЦЕЛИКОМ, плоским списком с отступами, поэтому потомки
// комментария — это идущие следом элементы с большей глубиной. Считать их
// заново незачем: число уже посчитано на сервере и стоит в «Ответы N».
(function () {
  'use strict';

  var list = document.querySelector('.thread:not(.linear)');
  if (!list) return;

  var items = Array.prototype.slice.call(list.querySelectorAll('.c'));
  var depth = function (el) { return parseInt(el.getAttribute('data-depth'), 10) || 1; };
  var folds = [];

  items.forEach(function (el, i) {
    var box = el.querySelector('.rcount');
    if (!box) return; // ветки нет — сворачивать нечего

    var d = depth(el), n = 0;
    for (var j = i + 1; j < items.length && depth(items[j]) > d; j++) n++;
    if (n === 0) return;

    var label = box.textContent;
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'fold';
    btn.setAttribute('aria-expanded', 'true');
    btn.textContent = label;
    btn.addEventListener('click', function () {
      var open = btn.getAttribute('aria-expanded') === 'true';
      for (var k = i + 1; k <= i + n; k++) items[k].hidden = open;
      btn.setAttribute('aria-expanded', open ? 'false' : 'true');
      btn.textContent = open ? 'Показать ' + label.toLowerCase() : label;
    });
    box.replaceWith(btn);
    folds.push(btn);
  });

  // «Свернуть все ответы» — как на НГС, справа от заголовка треда. Ставит его
  // скрипт, а не шаблон: без JS сворачивать нечем, а мёртвая ссылка на
  // странице хуже, чем её отсутствие.
  if (folds.length === 0) return;
  var head = document.querySelector('.cttl');
  if (!head) return;
  var all = document.createElement('button');
  all.type = 'button';
  all.className = 'fold foldall';
  all.setAttribute('aria-expanded', 'true');
  all.textContent = 'Свернуть все ответы';
  all.addEventListener('click', function () {
    var open = all.getAttribute('aria-expanded') === 'true';
    folds.forEach(function (b) {
      if ((b.getAttribute('aria-expanded') === 'true') === open) b.click();
    });
    all.setAttribute('aria-expanded', open ? 'false' : 'true');
    all.textContent = open ? 'Развернуть все ответы' : 'Свернуть все ответы';
  });
  head.appendChild(all);
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
