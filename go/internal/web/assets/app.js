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
  });
})();
