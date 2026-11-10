// 帮助说明页的交互:目录(窄屏折叠到顶部)、页内搜索、代理视图、语言切换。zh.html 与 en.html 共用,
// 界面文字都写在各自的 HTML 里(data-* 属性),这里不放任何文案。不请求任何东西,纯本页操作。
(function () {
  'use strict';
  var root = document.documentElement;
  var doc = document.getElementById('doc');
  var toc = document.getElementById('toc');
  var tocList = document.getElementById('toc-list');
  var tocBtn = document.getElementById('toc-btn');
  var input = document.getElementById('q');
  var clearBtn = document.getElementById('q-clear');
  var status = document.getElementById('status');
  var reseller = root.classList.contains('rv');
  var each = function (list, fn) { Array.prototype.forEach.call(list, fn); };
  var toArray = function (list) { return Array.prototype.slice.call(list); };

  // 当前视图里不显示的内容,目录和搜索都要跳过:代理视图藏 data-scope="admin",管理员视图藏 data-scope="reseller"
  var hiddenScope = reseller ? '[data-scope~="admin"]' : '[data-scope~="reseller"]';
  var scoped = function (el) { return !!(el && el.closest && el.closest(hiddenScope)); };
  var chapters = toArray(doc.querySelectorAll('.chap')).filter(function (c) { return !scoped(c); });
  var sectionsOf = function (c) { return toArray(c.children).filter(function (s) { return s.classList.contains('sec') && !scoped(s); }); };
  var titleOf = function (sec) { var h = sec.querySelector('h2, h3'); return h ? h.textContent.trim() : ''; };

  // ---- 目录 ----
  var links = {}; // id → 目录里的链接
  var items = {}; // id → 目录里的 li
  function buildToc() {
    var lastPart = null, ul = null;
    chapters.forEach(function (c) {
      var part = c.closest('.part');
      if (part !== lastPart || !ul) {
        lastPart = part;
        if (part && part.getAttribute('data-title')) {
          var g = document.createElement('div');
          g.className = 'grp';
          g.textContent = part.getAttribute('data-title');
          tocList.appendChild(g);
        }
        ul = document.createElement('ul');
        tocList.appendChild(ul);
      }
      var li = document.createElement('li');
      li.className = 'c';
      li.appendChild(tocLink(c.id, titleOf(c)));
      items[c.id] = li;
      var secs = sectionsOf(c);
      if (secs.length) {
        var su = document.createElement('ul');
        su.className = 's';
        secs.forEach(function (s) {
          var sl = document.createElement('li');
          sl.appendChild(tocLink(s.id, titleOf(s)));
          items[s.id] = sl;
          su.appendChild(sl);
        });
        li.appendChild(su);
      }
      ul.appendChild(li);
    });
  }
  function tocLink(id, text) {
    var a = document.createElement('a');
    a.href = '#' + id;
    a.textContent = text;
    links[id] = a;
    return a;
  }

  // 标题旁的 # 链接,方便复制某一节的地址
  function addAnchors() {
    each(doc.querySelectorAll('.chap > h2, .sec > h3'), function (h) {
      var id = h.parentNode.id;
      if (!id) return;
      var a = document.createElement('a');
      a.className = 'anchor';
      a.href = '#' + id;
      a.textContent = '#';
      a.setAttribute('aria-label', h.textContent.trim());
      h.appendChild(a);
    });
  }

  // ---- 当前位置:滚动时高亮目录里的对应项,并把地址栏的 #锚点 跟上(切换语言时停在同一节) ----
  var current = null;
  var searching = false;
  function headerHeight() { return document.querySelector('.head').getBoundingClientRect().height; }
  function spy() {
    var line = headerHeight() + 24, found = null;
    chapters.forEach(function (c) {
      if (c.hidden) return;
      if (c.getBoundingClientRect().top <= line) found = c;
      sectionsOf(c).forEach(function (s) { if (!s.hidden && s.getBoundingClientRect().top <= line) found = s; });
    });
    if (!found && chapters.length) found = chapters[0];
    if (!found || found === current) return;
    current = found;
    each(tocList.querySelectorAll('a.active'), function (a) { a.classList.remove('active'); });
    var chap = found.classList.contains('chap') ? found : found.closest('.chap');
    if (links[found.id]) links[found.id].classList.add('active');
    if (chap && links[chap.id]) links[chap.id].classList.add('active');
    if (!searching) {
      each(tocList.querySelectorAll('li.c.open'), function (li) { if (!chap || li !== items[chap.id]) li.classList.remove('open'); });
      if (chap && items[chap.id]) items[chap.id].classList.add('open');
    }
    keepVisible(links[found.id]);
    if (!searching && userScrolled && history.replaceState) history.replaceState(null, '', '#' + found.id);
  }
  // 目录自己滚到能看见当前项(只动目录,不动页面)
  function keepVisible(a) {
    if (!a || toc.classList.contains('open') || getComputedStyle(toc).position === 'fixed') return;
    var t = a.offsetTop - toc.offsetTop, h = toc.clientHeight;
    if (t < toc.scrollTop + 40) toc.scrollTop = Math.max(0, t - 40);
    else if (t > toc.scrollTop + h - 60) toc.scrollTop = t - h + 60;
  }
  var userScrolled = false;
  var ticking = false;
  window.addEventListener('scroll', function () {
    userScrolled = true;
    if (ticking) return;
    ticking = true;
    requestAnimationFrame(function () { ticking = false; spy(); });
  }, { passive: true });

  // ---- 窄屏:目录收在顶栏的按钮里 ----
  function setTocOpen(open) {
    toc.classList.toggle('open', open);
    if (tocBtn) tocBtn.setAttribute('aria-expanded', open ? 'true' : 'false');
  }
  if (tocBtn) tocBtn.addEventListener('click', function (e) { e.stopPropagation(); setTocOpen(!toc.classList.contains('open')); });
  toc.addEventListener('click', function (e) { if (e.target.closest('a')) setTocOpen(false); });
  document.addEventListener('click', function (e) {
    if (toc.classList.contains('open') && !toc.contains(e.target) && e.target !== tocBtn) setTocOpen(false);
  });

  // ---- 搜索:按小节过滤,多个词之间是"并且";命中的字用 CSS 高亮,不改正文 ----
  var norm = function (s) { return String(s).toLowerCase().replace(/\s+/g, ' ').trim(); };
  // 取一块的文字;代理视图里藏起来的部分不算
  function textOf(el) {
    var out = [];
    var walk = document.createTreeWalker(el, NodeFilter.SHOW_TEXT, null);
    for (var n = walk.nextNode(); n; n = walk.nextNode()) if (!scoped(n.parentNode)) out.push(n.data);
    return norm(out.join(' '));
  }
  var blocks = [];
  chapters.forEach(function (c) {
    var head = toArray(c.children).filter(function (x) { return !x.classList.contains('sec'); });
    blocks.push({ el: c, chap: c, head: true, nodes: head, text: norm(head.map(function (x) { return scoped(x) ? '' : textOf(x); }).join(' ')) });
    sectionsOf(c).forEach(function (s) { blocks.push({ el: s, chap: c, head: false, nodes: [s], text: textOf(s) }); });
  });

  var hasHighlight = typeof CSS !== 'undefined' && CSS.highlights && typeof Highlight === 'function';
  function clearMarks() { if (hasHighlight) CSS.highlights.delete('help-hit'); }
  function mark(nodes, terms) {
    if (!hasHighlight) return;
    var ranges = [];
    nodes.forEach(function (el) {
      var walk = document.createTreeWalker(el, NodeFilter.SHOW_TEXT, null);
      for (var n = walk.nextNode(); n && ranges.length < 3000; n = walk.nextNode()) {
        if (scoped(n.parentNode) || n.parentNode.closest('.anchor')) continue;
        var low = n.data.toLowerCase();
        terms.forEach(function (t) {
          for (var i = low.indexOf(t); i >= 0 && ranges.length < 3000; i = low.indexOf(t, i + t.length)) {
            var r = new Range();
            r.setStart(n, i);
            r.setEnd(n, i + t.length);
            ranges.push(r);
          }
        });
      }
    });
    try {
      if (ranges.length) CSS.highlights.set('help-hit', new Highlight(...ranges));
    } catch (e) { /* 高亮只是锦上添花,浏览器不支持时照样过滤 */ }
  }

  function resetSearch() {
    searching = false;
    clearMarks();
    status.hidden = true;
    chapters.forEach(function (c) { c.hidden = false; sectionsOf(c).forEach(function (s) { s.hidden = false; }); });
    each(tocList.querySelectorAll('li'), function (li) { li.hidden = false; });
    each(tocList.querySelectorAll('.grp'), function (g) { g.hidden = false; });
    each(tocList.querySelectorAll('li.c.open'), function (li) { li.classList.remove('open'); });
    current = null;
    spy();
  }

  function runSearch(q) {
    var terms = norm(q).split(' ').filter(Boolean);
    clearBtn.hidden = !q;
    if (!terms.length) { resetSearch(); return; }
    searching = true;
    clearMarks();
    var hits = 0, marked = [];
    var hit = function (b) { return terms.every(function (t) { return b.text.indexOf(t) >= 0; }); };
    chapters.forEach(function (c) {
      var own = blocks.filter(function (b) { return b.chap === c; });
      var headHit = hit(own[0]);
      var secHits = own.slice(1).filter(hit);
      var show = headHit || secHits.length > 0;
      c.hidden = !show;
      items[c.id].hidden = !show;
      items[c.id].classList.toggle('open', show);
      own.slice(1).forEach(function (b) {
        var visible = show && (headHit || secHits.indexOf(b) >= 0);
        b.el.hidden = !visible;
        if (items[b.el.id]) items[b.el.id].hidden = !visible;
      });
      if (headHit) { hits++; marked = marked.concat(own[0].nodes); }
      secHits.forEach(function (b) { hits++; marked.push(b.el); });
    });
    // 目录分组下面一个也没剩时,把分组名也藏起来
    each(tocList.querySelectorAll('.grp'), function (g) {
      var ul = g.nextElementSibling;
      g.hidden = !ul || !toArray(ul.children).some(function (li) { return !li.hidden; });
    });
    // 几个词连在一起出现时只高亮整句,免得 port 把 support 里的 port 也标出来
    var phrase = terms.join(' ');
    var phraseHit = terms.length > 1 && blocks.some(function (b) { return b.text.indexOf(phrase) >= 0; });
    mark(marked, phraseHit ? [phrase] : terms);
    status.hidden = false;
    var tpl = hits ? status.getAttribute('data-found') : status.getAttribute('data-none');
    status.querySelector('span').textContent = tpl.replace('{n}', hits).replace('{q}', q.trim());
    current = null;
    window.scrollTo(0, 0);
    spy();
  }

  var timer = null;
  input.addEventListener('input', function () {
    clearTimeout(timer);
    timer = setTimeout(function () { runSearch(input.value); }, 120);
  });
  input.addEventListener('keydown', function (e) {
    if (e.key === 'Escape') { input.value = ''; runSearch(''); input.blur(); }
  });
  var clearSearch = function () { input.value = ''; runSearch(''); input.focus(); };
  clearBtn.addEventListener('click', clearSearch);
  status.querySelector('button').addEventListener('click', clearSearch);
  // 和面板一样:按 / 聚焦搜索框
  document.addEventListener('keydown', function (e) {
    if (e.key === '/' && !e.ctrlKey && !e.metaKey && !e.altKey && !/^(INPUT|TEXTAREA|SELECT)$/.test((document.activeElement || {}).tagName || '')) {
      e.preventDefault();
      input.focus();
      input.select();
    }
    if (e.key === 'Escape' && toc.classList.contains('open')) setTocOpen(false);
  });

  // ---- 切换语言:带上视图参数和当前所在的小节 ----
  var langLink = document.getElementById('lang-link');
  if (langLink) langLink.addEventListener('click', function () {
    var base = langLink.getAttribute('href').split(/[?#]/)[0];
    langLink.href = base + location.search + (current ? '#' + current.id : location.hash);
  });

  buildToc();
  addAnchors();
  // 打开时带着 #锚点:等目录建好再定位一次(窄屏顶栏更高,浏览器自己那一下可能被挡住)
  if (location.hash) {
    var target = document.getElementById(decodeURIComponent(location.hash.slice(1)));
    if (target && !target.closest('[hidden]') && !scoped(target)) target.scrollIntoView();
  }
  spy();
})();
