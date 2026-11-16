import { state, load } from '../app.js';
import { get, post, SLOW } from '../api.js';
import { t } from '../i18n.js';
import { trErr } from '../errmsg.js';
import { esc, fmtDay, fmtDate, fmtRelative, toast, confirm, registerActions, badge, field, switchRow, openModal, copy, fv, fchk, cfTokenLink } from '../ui.js';
import { followPanel } from '../panelmove.js';
import { openCertDrawer, certLabel } from '../certpicker.js';
import { subDefaultOn, subSchemeChange } from '../subtls.js';

export const title = () => t('cert.title');
export const subtitle = () => t('cert.subtitle');
let data = null, pollTimer = null, source = '', panelChanged = false;

const certs = () => data.certs || [];
const certOf = target => certs().find(c => (target === 'lines' ? c.forLines : target === 'panel' ? c.forPanel : c.forSub));
const USES = { lines: 'cert.useLines', panel: 'cert.usePanel', sub: 'cert.useSub' };

// 签发日志是后端写的中文;英文界面按错误文案同一张表翻译,行首的时间不动
const logText = st => (st.log || []).map(l => { const m = /^(\d\d:\d\d:\d\d )([\s\S]*)$/.exec(l); return m ? m[1] + trErr(m[2]) : trErr(l); }).join('\n');

// 版式:证书页回答两个问题 —— 现在哪里用着哪张(三格:线路默认 / 面板 / 订阅,各带到期和「更换」),库里有哪些证书(一张表)。
// 「添加证书」是偶尔才做的事,收进弹框(以前常驻半个页面,左边三行、右边一长条表单,左下一大块空着)。
export async function render(el) {
  data = await get('cert');
  const s = data.settings, st = data.status;
  // 证书来源:有域名签发 / 无域名自签 / 服务器上已有的证书
  source = source || data.source || (s.webDomain ? 'acme' : 'selfsign');
  el.innerHTML = `
    <div id="c-txt"></div>
    ${notifyHint()}
    <section class="card">
      <div class="card-head"><h2>${t('cert.inUse')}</h2>${st.running ? badge(t('cert.running'), 'primary') : (st.lastOk ? badge(t('cert.lastOk') + ' ' + fmtRelative(st.lastOk), 'ok') : '')}</div>
      <div class="cu-grid">${['lines', 'panel', 'sub'].map(useTile).join('')}</div>
      ${panelChanged ? `<div class="hint warn-text row" style="margin-top:.7rem;justify-content:space-between;flex-wrap:wrap"><span>${t('cert.panelRestart')}</span><button class="btn sm" data-act="cert.restart">${t('cert.restartBtn')}</button></div>` : ''}
      <p class="hint">${t('cert.usesHelp')}</p>
    </section>
    <section class="card">
      <div class="card-head"><h2>${t('cert.store')}<span class="h-count">${certs().length || ''}</span></h2><button class="btn primary sm" data-act="cert.add" ${st.running ? 'disabled' : ''}>${t('cert.add')}</button></div>
      ${certs().length ? `<div class="table-wrap"><table class="grid" data-fit="cards:760">
        <thead><tr><th>${t('cert.subject')}</th><th class="c-p2">${t('cert.issuer')}</th><th>${t('cert.validity')}</th><th>${t('cert.renewCol')}</th><th>${t('cert.usedBy')}</th><th></th></tr></thead>
        <tbody>${certs().map(storeRow).join('')}</tbody>
      </table></div>` : `<div class="api-empty"><p>${esc(t('cert.storeEmpty'))}</p><button class="btn primary sm" data-act="cert.add">${t('cert.add')}</button></div>`}
    </section>
    <details class="card cert-log" ${st.running || st.lastError ? 'open' : ''}>
      <summary><h2>${t('cert.log')}</h2>${st.lastError ? badge(t('cert.lastFailed'), 'danger') : ''}</summary>
      <pre class="log" id="c-log">${esc(logText(st) || t('common.empty'))}</pre>
      ${st.lastError ? `<p class="hint" style="color:var(--danger)">${esc(trErr(st.lastError))}</p>` : ''}
    </details>`;
  renderTxt(st.txt);
  if (st.running) startPoll();
}

export function tick() {}

// 本机没配通知:续期失败、快到期的提醒发不出去,要说出来(副机的由主机代发,主机要配好)
function notifyHint() {
  if (data.notifyConfigured || !certs().length) return '';
  if (data.nodeMode) return `<p class="hint">${esc(t('cert.notifyNode'))}</p>`;
  return `<div class="hint warn-text row" style="margin-bottom:.8rem;justify-content:space-between;flex-wrap:wrap"><span>${esc(t('cert.notifyOff'))}</span><a class="btn sm" href="#/settings">${esc(t('cert.notifySet'))}</a></div>`;
}

// 「正在使用」的一格:线路默认 / 面板 / 订阅各选各的。上面是用途,中间是哪张证书和到期,右上角「更换」开抽屉
function useTile(target) {
  const c = certOf(target);
  const path = target === 'panel' ? data.panelCert : target === 'sub' ? data.subCert : data.certPath;
  let cur, tone = '';
  if (c) {
    cur = `<b class="cu-name" title="${esc(certLabel(c.name))}">${esc(certLabel(c.name))}</b>
      <span class="cu-meta">${certBadge(c.info, c)}<span class="muted small">${esc(sourceText(c))}${c.info.exists ? ' · ' + fmtDay(new Date(c.info.notAfter).getTime() / 1000) : ''}</span></span>`;
    if (!c.info.exists || c.info.daysLeft < 0) tone = ' bad'; else if (c.info.daysLeft <= 14) tone = ' warn';
  } else if (path && (target !== 'lines' || data.info.exists)) {
    cur = `<b class="cu-name mono small" title="${esc(path)}">${esc(path)}</b><span class="cu-meta"><span class="muted small">${t('cert.notInStore')}</span></span>`;
  } else {
    cur = `<b class="cu-name off">${t(target === 'lines' ? 'cert.none' : 'cert.noHttps')}</b><span class="cu-meta"><span class="muted small">${t(target === 'lines' ? 'cert.noneHint' : 'cert.noHttpsHint')}</span></span>`;
    if (target === 'lines') tone = ' warn';
  }
  return `<div class="cu-tile${tone}">
    <div class="cu-top"><span class="lb">${t(USES[target])}</span><button class="btn sm" data-act="cert.pick" data-id="${target}" ${certs().length ? '' : 'disabled'}>${t('cert.change')}</button></div>
    ${cur}
  </div>`;
}

function storeRow(c) {
  const i = c.info;
  const domains = (i.dnsNames || []).concat(i.ips || []);
  const uses = [];
  if (c.forLines) uses.push(badge(t('cert.useLines'), 'primary'));
  if (c.forPanel) uses.push(badge(t('cert.usePanel'), 'primary'));
  if (c.forSub) uses.push(badge(t('cert.useSub'), 'primary'));
  if (c.forReseller) uses.push(badge(t('cert.useReseller'), 'primary'));
  if ((c.lineNames || []).length) uses.push(`<span class="badge" title="${esc(c.lineNames.join('、'))}">${t('cert.nLines', { n: c.lineNames.length })}</span>`);
  return `<tr>
    <td class="primary-cell"><span class="pname" title="${esc(certLabel(c.name))}">${esc(certLabel(c.name))}</span>${domains.length && !(domains.length === 1 && domains[0] === c.name) ? `<div class="sub-cell">${domains.map(esc).join(', ')}</div>` : ''}</td>
    <td class="c-p2">${sourceBadge(c)}<div class="sub-cell">${esc(i.issuer || '')}</div></td>
    <td>${i.exists ? `${fmtDay(new Date(i.notAfter).getTime() / 1000)} ${certBadge(i, c)}` : `<span class="danger small">${esc(trErr(i.error || t('cert.none')))}</span>`}</td>
    <td>${renewCell(c)}</td>
    <td>${uses.join(' ') || '<span class="muted">—</span>'}</td>
    <td class="actions">
      ${c.source === 'acme' && !c.info.selfSigned ? `<button class="btn sm" data-act="cert.renew" data-id="${esc(c.name)}" title="${esc(t('cert.renewNowHelp'))}">${t('cert.renewBtn')}</button>` : ''}
      <button class="btn sm danger" data-act="cert.del" data-id="${esc(c.name)}">${t('common.delete')}</button>
    </td></tr>`;
}

// 「自动续期」一列:会不会自动续、什么时候续、上次结果;不能自动续的说清楚为什么(以前这些面板上一样都不显示)
const REASONS = {
  manual: ['cert.rManual', 'warn', 'cert.rManualHelp'],
  external: ['cert.rExternal', '', 'cert.rExternalHelp'],
  restored: ['cert.rRestored', 'warn', 'cert.rRestoredHelp'], // 换机还原放进来的副本:没有谁续它
  selfsign: ['cert.rSelf', '', ''],
  disabled: ['cert.rDisabled', 'warn', 'cert.rDisabledHelp'],
  missing: ['cert.rMissing', 'danger', 'cert.rMissingHelp'],
};
function renewCell(c) {
  const out = [];
  const toggle = on => `<button class="btn xs ghost" data-act="cert.autorenew" data-id="${esc(c.name)}" data-on="${on ? 1 : 0}">${t(on ? 'cert.rTurnOn' : 'cert.rTurnOff')}</button>`;
  if (c.renewable) {
    if (c.renewFails > 0) {
      out.push(badge(t('cert.rFailed', { n: c.renewFails }), 'danger'));
      out.push(`<div class="sub-cell danger-text">${esc(trErr(c.lastRenewErr || ''))}</div>`);
      out.push(`<div class="sub-cell">${esc(t('cert.rRetry', { t: fmtDate(c.nextCheckAt) }))}</div>`);
    } else if (c.overdue) {
      out.push(badge(t('cert.rDue'), 'warn'));
    } else {
      out.push(badge(t('cert.rAuto'), 'ok') + ` <span class="muted small">${esc(t('cert.rFrom', { date: fmtDay(c.renewAt) }))}</span>`);
    }
    if (c.warn && c.warn !== c.lastRenewErr) out.push(`<div class="sub-cell warn-text">${esc(trErr(c.warn))}</div>`); // 失败原因就是它的话不重复说
    if (c.lastRenewOk) out.push(`<div class="sub-cell">${esc(t('cert.rLastOk', { t: fmtRelative(c.lastRenewOk) }))}</div>`);
    out.push(`<div class="sub-cell">${toggle(false)}</div>`);
    return out.join('');
  }
  const [label, kind, help] = REASONS[c.reason] || ['cert.rExternal', '', ''];
  out.push(badge(t(label), kind));
  if (help) out.push(`<div class="sub-cell">${esc(t(help))}</div>`);
  if (c.reason === 'disabled') out.push(`<div class="sub-cell">${toggle(true)}</div>`);
  return out.join('');
}

const sourceText = c => ({ acme: "Let's Encrypt", selfsign: t('cert.srcSelf'), external: t('cert.srcExternal') })[c.source] || '';
function sourceBadge(c) {
  if (c.source === 'selfsign' || c.info.selfSigned) return badge(t('cert.srcSelf'), 'warn');
  if (c.source === 'external') return badge(t('cert.srcExternal'), 'primary');
  if (c.source === 'acme') return badge("Let's Encrypt", 'ok');
  return '';
}

// 添加之后顶替哪些用途:三个开关各一格(没开的不动)
function targetSwitches() {
  const hasReal = data.info.exists && !data.info.selfSigned;
  const self = source === 'selfsign';
  return `<h3 class="form-sec">${t('cert.applyTo')}</h3>
    <div class="ct-uses">
      ${switchRow('c-lines', t('cert.forLines'), !hasReal, t('cert.forLinesHelp'))}
      ${switchRow('c-panel', t('cert.applyPanel'), !self && !data.panelCert, t('cert.applyPanelHelp'))}
      ${switchRow('c-sub', t('cert.applySub'), subDefaultOn(data, self), data.subCert || !(data.subUsers > 0) ? t('cert.applySubHelp') : t('cert.applySubWarn', { n: data.subUsers }))}
    </div>`;
}

const SAVE_TEXT = { acme: 'cert.issueBtn', selfsign: 'cert.selfsign', external: 'cert.useExternal' };
// 「添加证书」弹框:最上面选来源(有域名自动签发 / 无域名自签 / 服务器上已有的),下面是这种来源要填的,最后是「添加后用于」。
// 订阅地址会跟着变、同名证书会被顶替这两种要再确认的情况:第一次点主按钮时在弹框里说明,再点一次才做(不另弹一个确认框把填好的表单盖掉)
let armed = '';
function addDialog() {
  armed = '';
  openModal(t('cert.add'), `<div class="ct-form">
      <div class="seg" id="c-source">${[['acme', t('cert.srcAcme')], ['selfsign', t('cert.srcSelf')], ['external', t('cert.srcExternal')]]
    .map(([k, label]) => `<button type="button" data-act="cert.source" data-id="${k}" class="${k === source ? 'active' : ''}">${label}</button>`).join('')}</div>
      <div id="c-form"></div>
    </div>`, submitAdd, { wide: true, saveText: t(SAVE_TEXT[source]) });
  renderForm();
}

// 三种来源各自的表单;共用下方的「添加后用于」
function renderForm() {
  const box = document.getElementById('c-form');
  if (!box) return;
  const s = data.settings;
  const save = document.getElementById('modal-save');
  if (save) save.textContent = t(SAVE_TEXT[source]);
  armed = '';
  if (source === 'acme') {
    const method = ['http', 'cloudflare', 'manual'].includes(s.acmeMethod) ? s.acmeMethod : 'http';
    box.innerHTML = `
      <div class="form-grid cols-2">
        <div class="full">${field(t('set.webDomain'), `<input id="c-domain" value="${esc(s.acmeDomain || s.webDomain || '')}" placeholder="hk.example.com / *.example.com">`, t('cert.domainHelp'))}</div>
        ${field(t('cert.method'), `<select id="c-method" data-change="cert.method">
          <option value="http" ${method === 'http' ? 'selected' : ''}>${t('cert.methodHttp')}</option>
          <option value="cloudflare" ${method === 'cloudflare' ? 'selected' : ''}>${t('cert.methodCf')}</option>
          <option value="manual" ${method === 'manual' ? 'selected' : ''}>${t('cert.methodManual')}</option></select>`, t('cert.methodHelp'))}
        ${field(t('cert.email'), `<input id="c-email" value="${esc(s.acmeEmail || '')}" placeholder="admin@example.com">`, t('cert.emailHelp'))}
        <div class="full" id="c-cf-wrap" ${method !== 'cloudflare' ? 'hidden' : ''}>${field(t('cert.cfToken'), `<input id="c-cftoken" type="password" placeholder="${s.hasCfToken ? t('cert.cfTokenKeep') : ''}">${cfTokenLink()}`, t('cert.cfTokenHelp'))}</div>
      </div>
      <div class="ct-check"><button type="button" class="btn sm" data-act="cert.precheck">${t('cert.precheck')}</button><div id="c-precheck"></div></div>
      <div class="ct-opts">
        <div id="c-renew-wrap">${switchRow('c-renew', t('cert.autoRenew'), s.acmeAutoRenew, t('cert.autoRenewHelp'))}<p class="hint danger-text" id="c-renew-manual" ${method === 'manual' ? '' : 'hidden'}>${esc(t('cert.manualNoRenew'))}</p></div>
        ${switchRow('c-staging', t('cert.staging'), s.acmeStaging, t('cert.stagingHelp'))}
      </div>
      ${targetSwitches()}`;
    document.getElementById('c-domain').addEventListener('input', syncWildcard);
    syncWildcard();
    syncMethod();
  } else if (source === 'selfsign') {
    // 无域名场景不该逼用户填东西:留空即用本机探测到的公网 IP,占位里先给他看是哪些
    const auto = [state.settings.publicIp, s.webDomain].filter(Boolean).join(', ');
    box.innerHTML = `
      <p class="hint">${t('cert.selfHelp')}</p>
      ${field(t('cert.hosts'), `<input id="ss-hosts" value="" placeholder="${esc(auto || '1.2.3.4')}">`, t('cert.hostsHelp'))}
      ${targetSwitches()}`;
  } else {
    box.innerHTML = `
      <p class="hint">${t('cert.externalHelp')}</p>
      <div class="form-grid cols-2">
        ${field(t('cert.certPath'), `<input id="ex-cert" value="" placeholder="/etc/letsencrypt/live/example.com/fullchain.pem">`)}
        ${field(t('cert.keyPath'), `<input id="ex-key" value="" placeholder="/etc/letsencrypt/live/example.com/privkey.pem">`, t('cert.pathHelp'))}
      </div>
      ${targetSwitches()}`;
  }
}

// 要再确认一次的事:第一次点时把话写在弹框的提示行里(抛出去由 openModal 显示),同一句话再点一次才放行
function confirmOnce(msg) {
  if (!msg || armed === msg) return;
  armed = msg;
  throw new Error(msg + ' ' + t('cert.againToConfirm'));
}

// 弹框的主按钮:按来源签发 / 生成自签 / 登记已有证书
async function submitAdd() {
  const use = targets();
  const subWarn = subSchemeText(use.applySub); // 订阅 http ↔ https 一换,所有用户的订阅地址都会变
  if (source === 'acme') {
    const method = fv('c-method');
    const body = { domain: fv('c-domain').trim(), email: fv('c-email').trim(), method, cfToken: fv('c-cftoken'),
      staging: fchk('c-staging'), autoRenew: method !== 'manual' && fchk('c-renew'), ...use };
    if (!body.domain) throw new Error(t('cert.needDomain'));
    confirmOnce(subWarn);
    const res = await post('cert/issue', body);
    toast(t(res && res.reason === 'manual' ? 'cert.startedManual' : 'cert.started'), 'ok');
    startPoll();
    await reload(); // 页面上显示「签发中」,日志跟着滚
    return;
  }
  if (source === 'selfsign') {
    confirmOnce(subWarn);
    await post('cert/selfsign', { hosts: fv('ss-hosts').trim(), ...use }); // 留空 = 后端自动用本机公网 IP
    toast(t('cert.done'), 'ok');
    await reload();
    return;
  }
  const body = { certFile: fv('ex-cert').trim(), keyFile: fv('ex-key').trim(), ...use };
  if (!body.certFile || !body.keyFile) throw new Error(t('cert.needPaths'));
  confirmOnce(subWarn);
  try { await post('cert/external', { ...body, force: forceShadow === body.certFile }); }
  catch (e) {
    // 同名证书是面板签发、开着自动续期的:换过去那张就不再自动续期,说明白、再点一次才换
    if (e.status !== 409) throw e;
    forceShadow = body.certFile;
    throw new Error(t('cert.shadowConfirm', { msg: e.message }) + ' ' + t('cert.againToConfirm'));
  }
  forceShadow = '';
  toast(t('cert.externalOk'), 'ok');
  await reload();
}
let forceShadow = '';

// 泛域名只能走 DNS 验证:HTTP 选项灰掉,已选 HTTP 的自动换走,说明换成泛域名那句
function syncWildcard() {
  const dom = document.getElementById('c-domain'), sel = document.getElementById('c-method');
  if (!dom || !sel) return;
  const wild = dom.value.trim().startsWith('*.');
  const http = sel.querySelector('option[value=http]');
  http.disabled = wild;
  if (wild && sel.value === 'http') {
    sel.value = data.settings.hasCfToken ? 'cloudflare' : 'manual';
    syncMethod();
    if (sel.value === 'manual') toast(t('cert.wildcardManual')); // 没有 Cloudflare Token:悄悄换成手动 DNS 的话,用户以为还会自动续
  }
  const help = sel.closest('.field').querySelector('.help');
  if (help) help.textContent = wild ? t('cert.wildcardHelp') : t('cert.methodHelp');
}
// 手动 DNS 不能自动续期:勾选框灰掉、取消勾选,下面红字说明(以前只是藏起来,勾着的 true 照样提交)
function syncMethod() {
  const v = fv('c-method');
  document.getElementById('c-cf-wrap').hidden = v !== 'cloudflare';
  const box = document.getElementById('c-renew'), manual = v === 'manual';
  if (box && manual && !box.disabled) { box.dataset.was = box.checked ? '1' : ''; box.checked = false; }
  if (box && !manual && box.disabled) box.checked = box.dataset.was === '1';
  if (box) box.disabled = manual;
  document.getElementById('c-renew-manual').hidden = !manual;
}

// 手动 DNS:要加的记录放在最显眼的地方,等它生效
function renderTxt(recs) {
  const box = document.getElementById('c-txt');
  if (!box) return;
  if (!recs || !recs.length) { box.innerHTML = ''; return; }
  box.innerHTML = `<section class="card cert-txt">
    <div class="card-head"><h2>${t('cert.txtTitle')}</h2><span class="row"><span class="badge primary">${t('cert.txtWaiting')}</span><button class="btn sm" data-act="cert.cancel">${t('cert.cancelIssue')}</button></span></div>
    <p class="hint">${t('cert.txtHelp')}</p>
    <div class="table-wrap"><table class="grid"><thead><tr><th>${t('cert.txtName')}</th><th>${t('cert.txtValue')}</th></tr></thead><tbody>
      ${recs.map(r => `<tr><td class="mono small">${esc(r.name)} <button class="btn xs ghost" data-act="cert.copy" data-v="${esc(r.name)}">${t('common.copy')}</button></td>
        <td class="mono small">${esc(r.value)} <button class="btn xs ghost" data-act="cert.copy" data-v="${esc(r.value)}">${t('common.copy')}</button></td></tr>`).join('')}
    </tbody></table></div></section>`;
}

// 剩余天数:会自动续的证书进了续期窗口还没续上,不再显示绿色(以前剩 23 天、天天续期失败也是绿的)
function certBadge(i, c) {
  if (!i.exists) return badge(t('cert.none'), 'warn');
  if (i.daysLeft < 0) return badge(t('cert.expired'), 'danger');
  if (i.daysLeft <= 14) return badge(t('cert.expiring', { n: i.daysLeft }), 'warn');
  if (c && c.renewable && c.overdue) return badge(t('cert.overdue', { n: i.daysLeft }), 'warn');
  return badge(t('cert.daysLeftN', { n: i.daysLeft }), 'ok');
}

// 上一次进度回来了再约下一次:setInterval 不等返回,弱网下请求叠起来,结束时还会弹两次提示、重绘两遍。
// 面板连不上时不能一直显示"申请中":连续失败一分钟就说清楚,签发本身还在后台跑。
let pollGen = 0;
function startPoll() {
  stopPoll();
  const gen = ++pollGen;
  let fails = 0, lastTxt = '';
  const step = async () => {
    const st = await get('cert/status').catch(() => null);
    if (gen !== pollGen) return; // 这期间被停了
    if (st) {
      fails = 0;
      const log = document.getElementById('c-log');
      if (log) { log.textContent = logText(st); log.scrollTop = log.scrollHeight; }
      const sig = JSON.stringify(st.txt || []);
      if (sig !== lastTxt) { lastTxt = sig; renderTxt(st.txt); }
      if (st.panelSwitch) { stopPoll(); followPanel(st.panelSwitch); return; } // 签好了,面板几秒后换到新地址
      if (!st.running) {
        stopPoll();
        toast(st.lastError ? t('cert.failed') : t('cert.done'), st.lastError ? 'err' : 'ok');
        await reload();
        return;
      }
    } else if (++fails >= 40) {
      stopPoll();
      toast(t('cert.pollLost'), 'err');
      return;
    }
    pollTimer = setTimeout(step, 1500);
  };
  pollTimer = setTimeout(step, 1500);
}
function stopPoll() { pollGen++; if (pollTimer) clearTimeout(pollTimer); pollTimer = null; }

async function reload() {
  await load('settings', 'status');
  const el = document.getElementById('page');
  if (el && location.hash.startsWith('#/cert')) await render(el);
}

const targets = () => ({ forLines: fchk('c-lines'), applyPanel: fchk('c-panel'), applySub: fchk('c-sub') });

// 订阅 http ↔ https 一换,所有用户手里的订阅地址都会变(见 subtls.js):返回要补进确认框的那句,不会变返回空
const subSchemeText = next => {
  const ch = subSchemeChange(data, next);
  return ch ? t('cert.subSchemeConfirm', { n: ch.n, from: ch.from, to: ch.to }) : '';
};

registerActions({
  'cert.source': id => {
    source = id;
    document.querySelectorAll('#c-source button').forEach(b => b.classList.toggle('active', b.dataset.id === id));
    renderForm();
  },
  'cert.method': () => syncMethod(),
  'cert.copy': async (_, el) => { await copy(el.dataset.v || ''); toast(t('common.copied'), 'ok'); },
  'cert.cancel': async () => {
    stopPoll(); // 取消之后轮询会看到一次"失败",这里自己收尾
    try { await post('cert/cancel'); toast(t('cert.cancelled'), 'ok'); } catch (e) { toast(e.message, 'err'); }
    setTimeout(reload, 800);
  },
  'cert.pick': target => {
    const opts = certs().map(c => ({
      value: c.name, name: certLabel(c.name), badge: certBadge(c.info, c),
      sub: esc(sourceText(c)) + ((c.info.dnsNames || []).length > 1 ? ' · ' + c.info.dnsNames.map(esc).join(', ') : ''),
      current: certOf(target) === c,
    }));
    if (target !== 'lines') opts.unshift({ value: '', name: t('cert.noHttps'), sub: esc(t('cert.noHttpsHelp')), current: !certOf(target) && !(target === 'panel' ? data.panelCert : data.subCert) });
    openCertDrawer(t('cert.pickFor.' + target), opts, async value => {
      if (target === 'lines' && !await confirm(t('cert.switchLinesConfirm'))) return false;
      const subWarn = target === 'sub' ? subSchemeText(value) : '';
      if (subWarn && !await confirm(subWarn, { danger: true })) return false;
      try {
        const r = await post('cert/use', { name: value, target }, SLOW);
        if (target === 'panel' && r && r.panelSwitch) followPanel(r.panelSwitch);
        else if (target === 'panel') panelChanged = true; // 老后端没有进程内换监听:照旧提示重启
        toast(t('cert.useOk'), 'ok');
        await reload();
        return true;
      } catch (e) { toast(e.message, 'err'); return false; }
    });
  },
  'cert.restart': async () => {
    if (!await confirm(t('bk.restartConfirm'), { danger: true })) return;
    await post('backup/restart').catch(() => {});
    toast(t('bk.restarting'), 'ok');
  },
  'cert.renew': async (name, btn) => {
    const c = certs().find(x => x.name === name);
    if (!c) return;
    if (!await confirm(t(c.method === 'manual' ? 'cert.renewManualConfirm' : 'cert.renewConfirm', { domain: name }))) return;
    btn.disabled = true;
    // 续期只换文件内容,路径不变,用途都不动;按这张自己的验证方式等设置续,不改签发表单的默认值
    try {
      await post('cert/renew', { name });
      toast(t('cert.started'), 'ok');
      startPoll();
    } catch (e) { toast(e.message, 'err'); btn.disabled = false; }
  },
  'cert.autorenew': async (name, btn) => {
    const on = btn.dataset.on === '1';
    if (!on && !await confirm(t('cert.rOffConfirm', { name: certLabel(name) }))) return;
    btn.disabled = true;
    try {
      await post('cert/autorenew', { name, on });
      toast(t(on ? 'cert.rOnOk' : 'cert.rOffOk'), 'ok');
      await reload();
    } catch (e) { toast(e.message, 'err'); btn.disabled = false; }
  },
  'cert.del': async name => {
    if (!await confirm(t('common.deleteConfirm', { name: certLabel(name) }), { danger: true, okText: t('common.delete') })) return;
    try { await post('cert/delete', { name }); toast(t('cert.deleted'), 'ok'); await reload(); }
    catch (e) { toast(e.message, 'err'); }
  },
  'cert.precheck': async (_, btn) => {
    const box = document.getElementById('c-precheck');
    btn.disabled = true;
    box.innerHTML = `<span class="muted small">${t('cert.checking')}</span>`;
    try {
      const p = await post('cert/precheck', { domain: fv('c-domain') }, SLOW);
      if (p.wildcard) { box.innerHTML = `<p class="hint">${t('cert.wildcardHelp')}</p>`; return; }
      box.innerHTML = `<dl class="kv">
        <dt>${t('cert.publicIp')}</dt><dd class="mono">${esc(p.publicIp || '—')}</dd>
        ${Object.entries(p.resolved).map(([r, v]) => `<dt class="mono">${esc(r)}</dt><dd class="mono">${esc(v)} ${v === p.publicIp ? badge('✓', 'ok') : badge('✗', 'danger')}</dd>`).join('')}
        <dt>${t('cert.port80')}</dt><dd>${p.port80 === 'free' ? badge(t('cert.port80Free'), 'ok') : badge(t('cert.port80Busy'), 'danger') + ` <span class="small muted">${esc(p.port80)}</span>`}</dd>
      </dl><p class="hint">${p.dnsOk ? t('cert.dnsOk') : t('cert.dnsBad')}</p>${p.wrongAaaa ? `<p class="hint warn">${esc(t('cert.aaaaBad', { ip: p.wrongAaaa }))}</p>` : ''}`;
    } catch (e) { box.innerHTML = `<span class="small" style="color:var(--danger)">${esc(e.message)}</span>`; }
    finally { btn.disabled = false; }
  },
  'cert.add': () => addDialog(),
});