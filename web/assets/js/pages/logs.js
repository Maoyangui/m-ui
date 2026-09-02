import { state, load } from '../app.js';
import { get, post, del } from '../api.js';
import { t } from '../i18n.js';
import { esc, fmtDate, registerActions, empty, debounce, confirm, toast } from '../ui.js';

export const title = () => t('logs.title');
let tab = 'sub', userFilter = '', level = 'info';

const logOn = () => String(state.settings.logEnabled ?? 'true') !== 'false';
const clearBtn = () => `<button class="btn sm danger" data-act="logs.clear">${t('logs.clear')}</button>`;
const logSwitch = () => `<label class="row" style="gap:.45rem;align-items:center;cursor:pointer" title="${esc(t('logs.enabledHelp'))}"><span class="switch"><input type="checkbox" id="logs-on" ${logOn() ? 'checked' : ''}><span></span></span><span class="small ${logOn() ? '' : 'muted'}">${t('logs.enabled')}</span></label>`;

export async function render(el) {
  el.innerHTML = `
    <div class="tabs">${['sub', 'core', 'audit'].map(k => `<button data-act="logs.tab" data-id="${k}" class="${k === tab ? 'active' : ''}">${t('logs.' + k)}</button>`).join('')}</div>
    <div id="logs-body"></div>`;
  await renderTab();
}

async function renderTab() {
  const el = document.getElementById('logs-body');
  if (!el) return;
  document.querySelectorAll('.tabs button').forEach(b => b.classList.toggle('active', b.dataset.id === tab));
  if (tab === 'sub') {
    el.innerHTML = `
      <div class="toolbar"><input type="search" id="logs-user" placeholder="${t('logs.filterUser')}" value="${esc(userFilter)}"><span class="grow"></span><button class="btn sm" data-act="logs.refresh">${t('common.refresh')}</button>${clearBtn()}</div>
      <div class="table-wrap"><table class="grid"><thead><tr><th>${t('logs.time')}</th><th>${t('logs.user')}</th><th>${t('logs.ip')}</th><th>${t('logs.format')}</th><th>${t('common.status')}</th><th>${t('logs.ua')}</th></tr></thead><tbody id="logs-rows"></tbody></table></div>`;
    document.getElementById('logs-user').addEventListener('input', debounce(e => { userFilter = e.target.value.trim(); loadSub(); }));
    await loadSub();
  } else if (tab === 'core') {
    el.innerHTML = `
      <div class="toolbar">${logSwitch()}<span class="grow"></span><select class="sm" id="logs-level" style="width:auto">${['debug', 'info', 'warning', 'error'].map(l => `<option ${l === level ? 'selected' : ''}>${l}</option>`).join('')}</select><button class="btn sm" data-act="logs.refresh">${t('common.refresh')}</button>${clearBtn()}</div>
      ${logOn() ? '' : `<p class="hint" style="margin:-.3rem 0 .7rem">${t('logs.enabledHelp')}</p>`}
      <pre class="log" id="logs-core" style="max-height:70vh"></pre>`;
    document.getElementById('logs-level').addEventListener('change', e => { level = e.target.value; loadCore(); });
    document.getElementById('logs-on').addEventListener('change', async e => {
      const on = e.target.checked;
      try { await post('logs', { enabled: String(on) }); await load('settings'); toast(t('logs.toggled'), 'ok'); await renderTab(); }
      catch (err) { toast(err.message, 'err'); e.target.checked = !on; }
    });
    await loadCore();
  } else {
    el.innerHTML = `
      <div class="toolbar"><span class="grow"></span><button class="btn sm" data-act="logs.refresh">${t('common.refresh')}</button>${clearBtn()}</div>
      <div class="table-wrap"><table class="grid"><thead><tr><th>${t('logs.time')}</th><th>${t('logs.actor')}</th><th>${t('logs.object')}</th><th>${t('logs.action')}</th><th>${t('common.details')}</th></tr></thead><tbody id="logs-rows"></tbody></table></div>`;
    await loadAudit();
  }
}

async function loadSub() {
  const rows = await get('sublogs?limit=300' + (userFilter ? '&user=' + encodeURIComponent(userFilter) : ''));
  const body = document.getElementById('logs-rows');
  if (!body) return;
  body.innerHTML = rows.map(l => `<tr><td class="num">${fmtDate(l.ts)}</td><td>${esc(l.user)}</td><td class="num">${esc(l.ip)}</td><td>${esc(l.format)}</td><td>${l.status}</td><td class="ua" title="${esc(l.ua)}">${esc(l.ua)}</td></tr>`).join('') || `<tr><td colspan="6">${empty()}</td></tr>`;
}
async function loadCore() {
  const lines = (await get(`logs?count=500&level=${level}`)) || [];
  const el = document.getElementById('logs-core');
  if (!el) return;
  el.innerHTML = lines.slice().reverse().map(l => {
    const cls = /ERROR/.test(l) ? 'error' : /WARN/.test(l) ? 'warn' : /DEBUG/.test(l) ? 'debug' : '';
    return `<span class="${cls}">${esc(l)}</span>`;
  }).join('\n') || t('common.empty');
}
async function loadAudit() {
  const rows = await get('audit?limit=300');
  const body = document.getElementById('logs-rows');
  if (!body) return;
  body.innerHTML = rows.map(r => {
    let obj = ''; try { obj = JSON.parse(r.obj); } catch { obj = r.obj; }
    if (Array.isArray(obj)) obj = obj.join(', ');
    if (obj === null) obj = '';
    return `<tr><td class="num">${fmtDate(r.dateTime)}</td><td>${esc(r.actor)}</td><td>${esc(r.key)}</td><td>${esc(r.action)}</td><td class="ua" title="${esc(String(obj))}">${esc(String(obj))}</td></tr>`;
  }).join('') || `<tr><td colspan="5">${empty()}</td></tr>`;
}

registerActions({
  'logs.tab': async id => { tab = id; await renderTab(); },
  'logs.refresh': async () => { await renderTab(); },
  'logs.clear': async () => {
    if (!await confirm(t('logs.clearConfirm'), { danger: true })) return;
    try { await del('logs?kind=' + tab); toast(t('logs.cleared'), 'ok'); await renderTab(); }
    catch (e) { toast(e.message, 'err'); }
  },
});
