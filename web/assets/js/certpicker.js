// 选证书的抽屉:证书页(线路默认 / 面板 / 订阅)和线路编辑框共用。一行一张证书,点一下就选。
// options: [{value, name, sub, badge, chips, current}];badge / chips 是拼好的 HTML,其余按文本转义。
// onPick(value) 返回 false 表示没选成(比如取消了确认框),抽屉留着。
import { esc, openDrawer, closeDrawer } from './ui.js';
import { t } from './i18n.js';

// 证书的显示名:自签证书在库里叫 self-signed,界面上写人话
export const certLabel = name => (name === 'self-signed' ? t('cert.selfName') : name);

export function openCertDrawer(title, options, onPick) {
  const rows = options.map((o, i) => `<button type="button" class="cert-opt ${o.current ? 'current' : ''}" data-i="${i}">
      <span class="cert-opt-check">${o.current ? '✓' : ''}</span>
      <span class="cert-opt-main">
        <b>${esc(o.name)}</b>${o.badge ? ' ' + o.badge : ''}
        ${o.sub ? `<span class="cert-opt-sub">${o.sub}</span>` : ''}
        ${o.chips ? `<span class="cert-opt-chips">${o.chips}</span>` : ''}
      </span>
    </button>`).join('');
  // 线路编辑框是弹窗,抽屉要盖在它上面
  openDrawer(esc(title), `<div class="cert-opts" id="cert-opts">${rows}</div>`, { over: true });
  const box = document.getElementById('cert-opts');
  box.addEventListener('click', async e => {
    const b = e.target.closest('.cert-opt');
    if (!b) return;
    box.querySelectorAll('.cert-opt').forEach(x => { x.disabled = true; });
    const ok = await onPick(options[Number(b.dataset.i)].value);
    if (ok === false) { box.querySelectorAll('.cert-opt').forEach(x => { x.disabled = false; }); return; }
    closeDrawer();
  });
}
