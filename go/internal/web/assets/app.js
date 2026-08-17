// Единственный скрипт площадки, и он ничего не создаёт — только сворачивает
// ветки в дереве. Всё остальное страница уже умеет без него: это условие, а не
// стиль, потому что строгий CSP запрещает inline-скрипты, а без внешних
// зависимостей (npm, CDN) их и взять неоткуда.
//
// Ветка вычисляется по разметке: потомки комментария — идущие следом элементы
// с большей глубиной. Отдельного дерева в DOM нет намеренно — плоский список с
// отступами это один range-scan на сервере и никакой рекурсии здесь.
(function () {
  'use strict';

  var list = document.querySelector('.thread:not(.flat)');
  if (!list) return;

  var items = Array.prototype.slice.call(list.querySelectorAll('.c'));
  var depth = function (el) { return parseInt(el.getAttribute('data-depth'), 10) || 1; };

  items.forEach(function (el, i) {
    var d = depth(el), n = 0;
    for (var j = i + 1; j < items.length && depth(items[j]) > d; j++) n++;
    if (n === 0) return;

    var head = el.querySelector('.chead') || el;
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'fold';
    btn.setAttribute('aria-expanded', 'true');
    btn.title = 'Свернуть ветку';
    btn.textContent = '−';

    btn.addEventListener('click', function () {
      var open = btn.getAttribute('aria-expanded') === 'true';
      for (var j = i + 1; j <= i + n; j++) items[j].hidden = open;
      btn.setAttribute('aria-expanded', open ? 'false' : 'true');
      btn.textContent = open ? '+' + n : '−';
      btn.title = open ? 'Развернуть ветку' : 'Свернуть ветку';
    });

    // Кнопка вставляется перед якорем, чтобы «#» оставался крайним справа.
    var anchor = head.querySelector('.anchor');
    if (anchor) head.insertBefore(btn, anchor); else head.appendChild(btn);
  });
})();
