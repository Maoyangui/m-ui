// 外部 API 的附加密钥与来源 IP 白名单:管理员页与代理面板「我的账号」共用。
// base 是接口前缀:主面板 'admin/api',代理 'self/api'。容器里放 <div id="api-keys"></div>,开启外部 API 后调用 mountApiKeys。
// 默认密钥(原来那一把)仍在页面上方单独显示,全部权限;这里只管附加密钥和白名单。
import { get, post, put, del } from './api.js';
import { t } from './i18n.js';
import { esc, toast, confirm, openModal, registerActions, badge, fv, fchk, copy, fmtTime, chipList, setHTML, field, check } from './ui.js';

const PERMS = ['users.read', 'users.create', 'users.update', 'users.delete', 'billing', 'plans.read', 'panel.read'];
let base = '', keys = [], ip = { enabled: false, list: '', you: '' };

const permChips = k => chipList((k.permList || []).map(p => `<span class="chip">${esc(t('api.perm.' + p))}</span>`), { key: 'apikey-' + k.id, max: 3 });
const masked = tok => (tok ? tok.slice(0, 6) + '…' + tok.slice(-4) : '');

function keysTable() {
  if (!keys.length) return `<p class="hint">${esc(t('api.keysNone'))}</p>`;
  return `<div class="table-wrap"><table class="grid api-keys"><thead><tr>
      <th>${t('api.keyName')}</th><th>${t('api.keyPerms')}</th><th>${t('api.keyLastUsed')}</th><th>${t('api.keyToken')}</th><th></th></tr></thead><tbody>
    ${keys.map(k => `<tr>
      <td class="primary-cell">${esc(k.name)}${k.enabled ? '' : ' ' + badge(t('common.disabled'))}</td>
      <td>${permChips(k)}</td>
      <td class="small">${k.lastUsedAt ? `${esc(fmtTime(k.lastUsedAt))}<div class="sub-cell muted">${esc(k.lastUsedIp || '')}</div>` : `<span class="muted">${esc(t('api.keyNever'))}</span>`}</td>
      <td><span class="nowrap"><code>${esc(masked(k.token))}</code> <button class="btn xs" data-act="apikey.copy" data-id="${k.id}">${t('common.copy')}</button></span></td>
      <td class="actions">
        <button class="btn sm" data-act="apikey.edit" data-id="${k.id}">${t('common.edit')}</button>
        <button class="btn sm" data-act="apikey.rotate" data-id="${k.id}">${t('api.keyRotate')}</button>
        <button class="btn sm danger" data-act="apikey.del" data-id="${k.id}">${t('common.delete')}</button>
      </td></tr>`).join('')}
  </tbody></table></div>`;
}

function ipBlock() {
  return `<div class="card-head" style="margin-top:1rem"><h3 class="sub-title" style="margin:0">${t('api.ip')}</h3>
      <label class="switch" title="${esc(t('api.ip'))}"><input type="checkbox" id="apiip-on" ${ip.enabled ? 'checked' : ''}><span></span></label></div>
    <p class="hint">${esc(t('api.ipHelp'))}</p>
    <textarea id="apiip-list" rows="2" placeholder="${esc(t('api.ipPlaceholder'))}" spellcheck="false">${esc((ip.list || '').split(', ').join('\n'))}</textarea>
    <div class="row" style="margin-top:.4rem;align-items:center;gap:.5rem;flex-wrap:wrap">
      <button class="btn primary sm" data-act="apiip.save">${t('common.save')}</button>
      ${ip.you ? `<span class="muted small">${esc(t('api.ipYou', { ip: ip.you }))}</span><button class="btn xs" data-act="apiip.addMe">${t('api.ipAddMe')}</button>` : ''}
    </div>`;
}

function draw() {
  setHTML('api-keys', `<div class="card-head"><h3 class="sub-title" style="margin:0">${t('api.keys')}</h3><button class="btn sm primary" data-act="apikey.add">${t('api.keyAdd')}</button></div>
    <p class="hint">${esc(t('api.keysHelp'))}</p>
    ${keysTable()}
    ${ipBlock()}`);
}

async function reload() {
  const [k, i] = await Promise.all([get(base + '/keys'), get(base + '/ip')]);
  keys = k.keys || [];
  ip = i || ip;
  draw();
}

export async function mountApiKeys(prefix) {
  base = prefix;
  try { await reload(); } catch (e) { setHTML('api-keys', `<p class="hint danger">${esc(e.message)}</p>`); }
}

function keyForm(k) {
  const have = new Set(k ? k.permList : ['users.read']);
  return `<div class="form-grid">${field(t('api.keyName'), `<input id="ak-name" maxlength="40" value="${esc(k ? k.name : '')}" placeholder="${esc(t('api.keyNamePh'))}">`)}</div>
    <div class="perm-grid">${PERMS.map(p => `<label class="perm-item"><input type="checkbox" class="ak-perm" value="${p}" ${have.has(p) ? 'checked' : ''}>
      <span><b>${esc(t('api.perm.' + p))}</b><small class="muted">${esc(t('api.permHelp.' + p))}</small></span></label>`).join('')}</div>
    ${check('ak-enabled', t('api.keyEnabled'), !k || k.enabled)}`;
}

const formBody = () => ({ name: fv('ak-name').trim(), enabled: fchk('ak-enabled'), perms: [...document.querySelectorAll('.ak-perm:checked')].map(x => x.value) });

registerActions({
  'apikey.add': () => openModal(t('api.keyAdd'), keyForm(null), async () => {
    const k = await post(base + '/keys', formBody());
    toast(t('api.keyCreated', { name: k.name }), 'ok');
    await reload();
  }),
  'apikey.edit': id => {
    const k = keys.find(x => String(x.id) === String(id));
    if (!k) return;
    openModal(t('api.keyEdit'), keyForm(k), async () => {
      await put(base + '/keys/' + k.id, formBody());
      toast(t('api.keySaved'), 'ok');
      await reload();
    });
  },
  'apikey.copy': id => { const k = keys.find(x => String(x.id) === String(id)); if (k) copy(k.token); },
  'apikey.rotate': async id => {
    const k = keys.find(x => String(x.id) === String(id));
    if (!k || !await confirm(t('api.keyRotateConfirm', { name: k.name }), { danger: true })) return;
    try { await post(base + '/keys/' + k.id + '/rotate'); toast(t('api.keyRotated'), 'ok'); await reload(); }
    catch (e) { toast(e.message, 'err'); }
  },
  'apikey.del': async id => {
    const k = keys.find(x => String(x.id) === String(id));
    if (!k || !await confirm(t('api.keyDeleteConfirm', { name: k.name }), { danger: true })) return;
    try { await del(base + '/keys/' + k.id); toast(t('common.deleted'), 'ok'); await reload(); }
    catch (e) { toast(e.message, 'err'); }
  },
  'apiip.addMe': () => {
    const el = document.getElementById('apiip-list');
    if (!el || !ip.you) return;
    const have = el.value.split(/[\s,，;]+/).filter(Boolean);
    if (!have.includes(ip.you)) el.value = [...have, ip.you].join('\n');
  },
  'apiip.save': async () => {
    try {
      ip = await put(base + '/ip', { enabled: fchk('apiip-on'), list: fv('apiip-list') });
      toast(t('api.ipSaved'), 'ok');
      draw();
    } catch (e) { toast(e.message, 'err'); }
  },
});
