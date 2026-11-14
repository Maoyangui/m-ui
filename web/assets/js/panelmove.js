// 面板换了监听(端口、监听地址、证书 / 开关 HTTPS)之后跟到新地址。后端在进程里换监听,不重启 m-ui(web/panel_switch.go),
// 设置页、证书页「用于面板」的回复和证书签发进度里带着 panelSwitch {scheme, host, port, path, at}:
// 到预计换好的时刻起试着连新地址,能连上就跳过去(带上现在的页面位置);一直连不上(证书不受信任、防火墙没放行新端口、
// 域名还没指向这台)就停下,给出新地址让人自己点。https 的页面探测不了 http 的地址(混合内容会被浏览器拦下),到点直接跳。
import { t } from './i18n.js';
import { esc, toast } from './ui.js';

let following = false;

// panelURL 新地址;没给主机名的沿用现在浏览器里的(没设面板域名,或换回 http)
export function panelURL(sw) {
  const host = sw.host || location.hostname;
  const h = host.includes(':') && !host.startsWith('[') ? '[' + host + ']' : host;
  const def = sw.scheme === 'https' ? 443 : 80;
  return `${sw.scheme}://${h}${Number(sw.port) === def ? '' : ':' + sw.port}${sw.path || '/'}`;
}

export function followPanel(sw) {
  if (!sw || following) return;
  const url = panelURL(sw);
  const target = url + (location.hash || '');
  if (url === location.origin + location.pathname) { // 地址没变(只换了证书):原地换好,页面照常能用
    setTimeout(() => toast(t('pm.same'), 'ok'), Math.max(0, sw.at - Date.now()) + 500);
    return;
  }
  following = true;
  const box = document.createElement('div');
  box.className = 'modal pm-mask';
  box.innerHTML = `<div class="modal-box pm-box" role="alertdialog" aria-live="polite">
    <h2>${esc(t('pm.title'))}</h2>
    <div class="pm-body">
      <a class="pm-url mono" href="${esc(target)}">${esc(url)}</a>
      <p class="pm-state"><span class="pm-spin" aria-hidden="true"></span><span id="pm-msg">${esc(t('pm.waiting'))}</span></p>
      ${sw.host && sw.host !== location.hostname ? `<p class="hint">${esc(t('pm.relogin'))}</p>` : ''}
    </div>
    <div class="pm-foot"><button class="btn" id="pm-stay">${esc(t('pm.stay'))}</button><a class="btn primary" href="${esc(target)}">${esc(t('pm.open'))}</a></div>
  </div>`;
  document.body.appendChild(box);
  let stopped = false;
  box.querySelector('#pm-stay').addEventListener('click', () => { stopped = true; following = false; box.remove(); });
  const wait = Math.max(0, sw.at - Date.now()) + 400;
  const deadline = Date.now() + wait + 45000;
  const mixed = location.protocol === 'https:' && sw.scheme === 'http';
  const probe = async () => {
    if (stopped) return;
    if (mixed) { location.href = target; return; }
    try {
      await fetch(url + 'api/status', { mode: 'no-cors', cache: 'no-store' });
      location.href = target;
    } catch {
      if (Date.now() > deadline) {
        box.querySelector('.pm-spin').remove();
        const msg = box.querySelector('#pm-msg');
        msg.textContent = t('pm.failed');
        msg.parentElement.classList.add('warn-text');
        return;
      }
      setTimeout(probe, 1500);
    }
  };
  setTimeout(probe, wait);
}
