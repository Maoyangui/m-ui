import { state, load } from '../app.js';
import { get, post, del, upload } from '../api.js';
import { t } from '../i18n.js';
import { esc, fmtBytes, fmtDate, fmtRelative, toast, confirm, openModal, closeModal, registerActions, badge, field, check, empty, fv, fchk, copy } from '../ui.js';

export const title = () => t('bk.title');
export const subtitle = () => t('bk.subtitle');
let data = null;

export async function render(el) {
  data = await get('backup/list');
  data.files = data.files || [];
  el.innerHTML = `
    ${data.restorePending ? `<div class="card" style="border-color:var(--warn)"><b>${t('bk.pending')}</b> <button class="btn sm" data-act="bk.restart">${t('bk.restartNow')}</button></div>` : ''}
    <div class="grid-2">
      <section class="card">
        <div class="card-head"><h2>${t('bk.now')}</h2></div>
        <p class="hint">${t('bk.nowHelp')}</p>
        <div class="row" style="margin-top:.8rem;flex-wrap:wrap">
          <a class="btn primary" href="./api/backup" download>${t('bk.download')}</a>
          <button class="btn" data-act="bk.run">${t('bk.saveLocal')}</button>
        </div>
      </section>
      <section class="card">
        <div class="card-head"><h2>${t('bk.restore')}</h2></div>
        <p class="hint">${t('bk.restoreHelp')}</p>
        <div class="row" style="margin-top:.8rem;flex-wrap:wrap">
          <input type="file" id="bk-file" accept=".zip,.db" style="max-width:18rem">
          <button class="btn danger" data-act="bk.upload">${t('bk.restoreBtn')}</button>
        </div>
      </section>
    </div>
    <section class="card">
      <div class="card-head"><h2>${t('bk.schedule')}</h2><button class="btn primary sm" data-act="bk.saveSchedule">${t('common.save')}</button></div>
      <div class="form-grid">
        ${field(t('bk.hour'), `<input id="bk-hour" type="number" min="-1" max="23" value="${esc(data.backupHour || '')}" placeholder="4">`, t('bk.hourHelp'))}
        ${field(t('bk.keep'), `<input id="bk-keep" type="number" min="1" value="${esc(data.backupKeep || '')}" placeholder="7">`)}
        ${check('bk-tg', t('bk.telegram'), data.backupTelegram, t('bk.telegramHelp'))}
      </div>
      <p class="hint mono">${esc(data.dir)}</p>
    </section>
    <section class="card">
      <div class="card-head"><h2>${t('mig.title')}</h2><span class="badge">${t('mig.badge')}</span></div>
      <p class="hint">${t('mig.help')}</p>
      <ol class="mig-steps">
        <li><b>${t('mig.s1')}</b><div class="hint">${t('mig.s1h')}</div>
          <div class="sub-box"><code>bash install.sh ./m-ui-linux-amd64 --restore m-ui-backup.zip</code><button class="btn sm" data-act="bk.copyCmd">${t('common.copy')}</button></div></li>
        <li><b>${t('mig.s2')}</b><div class="hint">${t('mig.s2h')}</div></li>
        <li><b>${t('mig.s3')}</b><div class="hint">${t('mig.s3h')}</div>
          <div class="row" style="margin-top:.4rem;flex-wrap:wrap">
            <input id="mig-domain" placeholder="${esc(state.settings.webDomain || 'hk.example.com')}" value="${esc(state.settings.webDomain || '')}" style="max-width:14rem">
            <input id="mig-ip" placeholder="${t('mig.newIp')}" style="max-width:12rem">
            <button class="btn sm" data-act="bk.dnsCheck">${t('mig.check')}</button>
          </div>
          <div id="mig-result" style="margin-top:.4rem"></div></li>
        <li><b>${t('mig.s4')}</b><div class="hint">${t('mig.s4h')}</div></li>
      </ol>
    </section>
    <section class="card">
      <div class="card-head"><h2>${t('bk.local')}</h2></div>
      <div class="table-wrap"><table class="grid">
        <thead><tr><th>${t('common.name')}</th><th>${t('bk.size')}</th><th>${t('bk.time')}</th><th></th></tr></thead>
        <tbody>${data.files.length ? data.files.map(f => `<tr>
          <td class="mono">${esc(f.name)}</td><td class="num">${fmtBytes(f.size)}</td><td>${fmtDate(f.time)} <span class="muted small">${fmtRelative(f.time)}</span></td>
          <td class="actions">
            <a class="btn sm" href="./api/backup/file?name=${encodeURIComponent(f.name)}" download>${t('bk.download')}</a>
            <button class="btn sm" data-act="bk.restoreLocal" data-id="${esc(f.name)}">${t('bk.restoreBtn')}</button>
            <button class="btn sm danger" data-act="bk.del" data-id="${esc(f.name)}">${t('common.delete')}</button>
          </td></tr>`).join('') : `<tr><td colspan="4">${empty(t('bk.noLocal'))}</td></tr>`}</tbody>
      </table></div>
    </section>`;
}

export function tick() {}

function summaryHTML(s) {
  return `<dl class="kv">
    <dt>${t('bk.from')}</dt><dd>${esc(s.meta?.host || '—')} · ${s.meta?.version ? 'm-ui ' + esc(s.meta.version) : t('bk.rawDb')} ${s.meta?.time ? `· ${fmtDate(s.meta.time)}` : ''}</dd>
    <dt>${t('nav.users')}</dt><dd class="num">${s.users}</dd>
    <dt>${t('nav.lines')}</dt><dd class="num">${s.lines}</dd>
    <dt>${t('nav.upstreams')}</dt><dd class="num">${s.upstreams}</dd>
    <dt>${t('cert.title')}</dt><dd class="num">${(s.meta?.certs || []).length}</dd>
  </dl><p class="hint" style="color:var(--danger)">${t('bk.restoreWarn')}</p>`;
}

async function waitBack() {
  toast(t('bk.restarting'), 'ok');
  const start = Date.now();
  while (Date.now() - start < 60000) {
    await new Promise(r => setTimeout(r, 2000));
    try { await fetch('./api/status', { cache: 'no-store' }); location.reload(); return; } catch {}
  }
  toast(t('bk.restartTimeout'), 'err');
}

registerActions({
  'bk.run': async (_, btn) => {
    btn.disabled = true;
    try { const f = await post('backup/run'); toast(t('bk.saved', { name: f.name }), 'ok'); render(document.getElementById('page')); }
    catch (e) { toast(e.message, 'err'); }
    finally { btn.disabled = false; }
  },
  'bk.saveSchedule': async () => {
    try {
      await post('settings', { backupHour: fv('bk-hour').trim(), backupKeep: fv('bk-keep').trim(), backupTelegram: String(fchk('bk-tg')) });
      await load('settings'); toast(t('set.saved'), 'ok');
    } catch (e) { toast(e.message, 'err'); }
  },
  'bk.del': async name => {
    if (!await confirm(t('common.deleteConfirm', { name }), { danger: true, okText: t('common.delete') })) return;
    try { await del('backup/file?name=' + encodeURIComponent(name)); render(document.getElementById('page')); }
    catch (e) { toast(e.message, 'err'); }
  },
  'bk.upload': async () => {
    const f = document.getElementById('bk-file').files[0];
    if (!f) { toast(t('bk.pickFile'), 'err'); return; }
    try {
      const s = await upload('backup/inspect', f);
      openModal(t('bk.confirmTitle'), summaryHTML(s), async () => {
        await upload('backup/restore', f);
        closeModal();
        waitBack();
      }, { okText: t('bk.restoreBtn'), danger: true });
    } catch (e) { toast(e.message, 'err'); }
  },
  'bk.restoreLocal': async name => {
    try {
      const s = await post('backup/inspect', { name });
      openModal(t('bk.confirmTitle') + ' · ' + name, summaryHTML(s), async () => {
        await post('backup/restore', { name });
        closeModal();
        waitBack();
      }, { okText: t('bk.restoreBtn'), danger: true });
    } catch (e) { toast(e.message, 'err'); }
  },
  'bk.copyCmd': () => copy('bash install.sh ./m-ui-linux-amd64 --restore m-ui-backup.zip'),
  'bk.dnsCheck': async (_, btn) => {
    const domain = fv('mig-domain').trim(), ip = fv('mig-ip').trim();
    const box = document.getElementById('mig-result');
    if (!domain) { toast(t('cert.needDomain'), 'err'); return; }
    btn.disabled = true;
    box.innerHTML = `<span class="muted small">${t('cert.checking')}</span>`;
    try {
      const p = await post('cert/precheck', { domain });
      const target = ip || p.publicIp;
      const rows = Object.entries(p.resolved).map(([r, v]) => `<dt class="mono">${esc(r)}</dt><dd class="mono">${esc(v)} ${v === target ? badge('✓', 'ok') : badge('✗', 'danger')}</dd>`).join('');
      const all = Object.values(p.resolved).every(v => v === target);
      box.innerHTML = `<dl class="kv">${rows}</dl><p class="hint">${all ? t('mig.dnsOk', { ip: target }) : t('mig.dnsWait', { ip: target })}</p>`;
    } catch (e) { box.innerHTML = `<span class="small" style="color:var(--danger)">${esc(e.message)}</span>`; }
    finally { btn.disabled = false; }
  },
  'bk.restart': async () => {
    if (!await confirm(t('bk.restartConfirm'), { danger: true })) return;
    await post('backup/restart').catch(() => {});
    waitBack();
  },
});
