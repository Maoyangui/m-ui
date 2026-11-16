// 备份页。从上到下三段,每段通栏或两张等高的卡并排,不嵌套方块:
//   1. 「备份」—— 这一页的主体就是服务器上现有的备份这张表;右上角两个按钮(存一份到服务器、下载一份),表下面一行小字写存在哪、一共多大。
//      (以前这张表在页面最底下,和「立即备份」隔着整个换机向导。)
//   2. 「定时备份」和「还原」两张卡并排、一样高:
//      定时备份 —— 标题右边是总开关,下面是一句话「每天 [04:00] 自动备份,保留最近 [7] 份」和「同时推送到 Telegram」的开关,改了当场保存;
//      还原 —— 一句话说明后果,一个「选择备份文件」按钮(选完先看里面有什么,确认了才动)。
//   3. 「换到新服务器」—— 平时用不到,收成一行,点开是四步(带序号的竖排步骤)。
// 排版上走过两次弯路,记在这里免得再走:① 把定时备份的四项收进弹框 —— 「同时推送到 Telegram」从页面上消失,要点「设置」才找得到,常用的开关不该藏;
// ② 备份、定时备份左右并排 —— 两张卡高矮不齐,右边窄栏里每项的说明挤成两三行,再加两个又大又空的灰色方块,整页又重又乱。
// 定时备份以前是一个「每天几点(-1 关闭)」的数字框,框里灰色的 4 看着像默认 4 点在备份 —— 其实留空就是没开,很多人以为开着;现在是开关。
import { state, load } from '../app.js';
import { get, post, del, upload, SLOW } from '../api.js';
import { t } from '../i18n.js';
import { trErr } from '../errmsg.js';
import { esc, fmtBytes, fmtDate, fmtRelative, toast, confirm, openModal, closeModal, registerActions, badge, fv, fchk, copy, getTimezone, setHTML } from '../ui.js';

export const title = () => t('bk.title');
export const subtitle = () => t('bk.subtitle');
let data = null;

// 换机第一步:新机上还没有 install.sh,直接取仓库里的一键安装脚本(自动分 amd64 / arm64,装最新版),再还原备份。
// 备份文件名按 m-ui-时间.zip 举例,界面上提示换成自己的路径
const migCmd = () => `bash <(curl -fsSL ${String(state.status.repo || 'https://github.com/Maoyangui/m-ui').replace(/^https:\/\/github\.com\//, 'https://raw.githubusercontent.com/')}/main/deploy/install.sh) --restore /root/m-ui-20261111-030000.zip`;

// 定时备份现在的设置:钟点是 0–23 的整数才算开着(留空、-1、乱填都是没开,和后端 backupLoop 同一个判断)
function sched() {
  const h = /^\s*\d+\s*$/.test(String(data.backupHour ?? '')) ? Number(data.backupHour) : -1;
  const on = h >= 0 && h <= 23;
  return { on, hour: on ? h : 4, keep: Number(data.backupKeep) > 0 ? Math.floor(Number(data.backupKeep)) : 7, tg: !!data.backupTelegram };
}
const hh = h => String(h).padStart(2, '0') + ':00';
const isPre = name => !String(name).startsWith('m-ui-'); // 升级前自动留的那几份(不参与按份数轮转)

// 把一句带 {time}、{keep} 这类占位的话拆开:占位处放控件,其余文字转义(中英文语序不一样,控件跟着句子走)。
// 句子里的 | 是可以折行的地方:每一段自己不折行,窄屏上整段换到下一行(不然「份」字会单独掉到第二行)
const sentence = (tpl, parts) => tpl.split('|').map(seg => '<span class="nw">' + seg.split(/(\{\w+\})/).map(s => {
  const m = /^\{(\w+)\}$/.exec(s);
  return m && parts[m[1]] !== undefined ? parts[m[1]] : (s.trim() ? `<span>${esc(s.trim())}</span>` : '');
}).join('') + '</span>').join('');

// 定时备份:标题右边是总开关;下面一句话里选几点、留几份,再一行是「同时推送到 Telegram」。没开的时候下面置灰(还看得见,知道开了以后能调什么)
function autoHTML() {
  const sc = sched(), dis = sc.on ? '' : ' disabled';
  const hours = Array.from({ length: 24 }, (_, h) => `<option value="${h}"${h === sc.hour ? ' selected' : ''}>${hh(h)}</option>`).join('');
  const line = sentence(t('bk.sched.line'), {
    time: `<select id="bk-hour" data-change="bk.sched" aria-label="${esc(t('bk.sched.hour'))}"${dis}>${hours}</select>`,
    keep: `<input id="bk-keep" type="number" min="1" max="365" inputmode="numeric" data-change="bk.sched" aria-label="${esc(t('bk.keep'))}" value="${sc.keep}"${dis}>`,
  });
  const fail = sc.on && data.lastErr ? `<p class="bk-fail">${esc(t('bk.t.failed', { msg: trErr(data.lastErr) }))}${data.lastErrAt ? ` <span class="muted">${esc(fmtRelative(data.lastErrAt))}</span>` : ''}</p>` : '';
  const tgWarn = sc.on && sc.tg && !data.tgReady;
  return `<div class="card-head"><h2>${t('bk.schedule')}${sc.on ? '' : `<span class="h-count">${esc(t('bk.t.off'))}</span>`}</h2>
      <label class="switch" title="${esc(t('bk.sched.on'))}"><input type="checkbox" id="bk-on" data-change="bk.sched" aria-label="${esc(t('bk.sched.on'))}" ${sc.on ? 'checked' : ''}><span></span></label></div>
    ${fail}
    <div class="bk-auto-body${sc.on ? '' : ' off'}">
      <p class="bk-line">${line}</p>
      <p class="hint">${esc(t('bk.sched.note', { tz: getTimezone() || 'Asia/Shanghai' }))}</p>
    </div>
    <label class="bk-tg${sc.on ? '' : ' off'}"><span class="switch"><input type="checkbox" id="bk-tg" data-change="bk.sched" ${sc.tg ? 'checked' : ''}${dis}><span></span></span>
      <span class="tx"><b>${esc(t('bk.telegram'))}</b><small${tgWarn ? ' class="warn-text"' : ''}>${esc(data.tgReady ? t('bk.telegramHelp') : t('bk.telegramNeed'))}</small></span></label>`;
}

function listHTML() {
  if (!data.files.length) return `<p class="bk-none">${esc(t('bk.noLocal'))}</p>`;
  const total = data.files.reduce((a, f) => a + (f.size || 0), 0);
  return `<div class="table-wrap bk-list"><table class="grid" data-fit="cards:620">
    <thead><tr><th>${t('common.name')}</th><th>${t('bk.size')}</th><th>${t('bk.time')}</th><th></th></tr></thead>
    <tbody>${data.files.map(f => `<tr>
      <td class="primary-cell"><span class="pname mono" title="${esc(f.name)}">${esc(f.name)}</span>${isPre(f.name) ? ` <span class="badge" title="${esc(t('bk.preTitle'))}">${esc(t('bk.pre'))}</span>` : ''}</td>
      <td class="num">${fmtBytes(f.size, 1)}</td>
      <td>${fmtDate(f.time)} <span class="muted small">${fmtRelative(f.time)}</span></td>
      <td class="actions">
        <a class="btn sm" href="./api/backup/file?name=${encodeURIComponent(f.name)}" download>${t('bk.download')}</a>
        <button class="btn sm" data-act="bk.restoreLocal" data-id="${esc(f.name)}">${t('bk.restoreBtn')}</button>
        <button class="btn sm danger" data-act="bk.del" data-id="${esc(f.name)}">${t('common.delete')}</button>
      </td></tr>`).join('')}</tbody>
  </table></div>
  <p class="bk-foot">${esc(t('bk.foot', { size: fmtBytes(total, 1) }))} <span class="mono">${esc(data.dir || '')}</span></p>`;
}

function migHTML() {
  const step = (n, head, body) => `<li><span class="n">${n}</span><div><b>${esc(head)}</b>${body}</div></li>`;
  return `<ol class="bk-steps">
    ${step(1, t('mig.s1'), `<p class="hint">${esc(t('mig.s1h'))}</p>
      <div class="sub-box"><code>${esc(migCmd())}</code><button type="button" class="btn sm" data-act="bk.copyCmd">${t('common.copy')}</button></div>
      <p class="hint">${esc(t('mig.cmdHelp'))}</p>`)}
    ${step(2, t('mig.s2'), `<p class="hint">${esc(t('mig.s2h'))}</p>`)}
    ${step(3, t('mig.s3'), `<p class="hint">${esc(t('mig.s3h'))}</p>
      <div class="bk-dns">
        <input id="mig-domain" placeholder="${esc(state.settings.webDomain || 'hk.example.com')}" value="${esc(state.settings.webDomain || '')}" aria-label="${esc(t('mig.domain'))}">
        <input id="mig-ip" placeholder="${esc(t('mig.newIp'))}" aria-label="${esc(t('mig.newIp'))}">
        <button type="button" class="btn sm" data-act="bk.dnsCheck">${t('mig.check')}</button>
      </div>
      <div id="mig-result"></div>`)}
    ${step(4, t('mig.s4'), `<p class="hint">${esc(t('mig.s4h'))}</p>`)}
  </ol>`;
}

function draw(el = document.getElementById('page')) {
  if (!el || !data) return;
  const open = !!el.querySelector('details.bk-mig[open]'); // 重画时(存了一份、删了一份)换机步骤开着就别收起来
  setHTML(el, `
    ${data.restorePending ? `<div class="alert-bar warn bk-pending"><span>${esc(t('bk.pending'))}</span><button class="btn sm" data-act="bk.restart">${t('bk.restartNow')}</button></div>` : ''}
    <section class="card bk-main">
      <div class="card-head"><h2>${t('bk.card')}<span class="h-count">${data.files.length || ''}</span></h2>
        <div class="head-acts">
          <button type="button" class="btn sm" data-act="bk.run">${t('bk.saveLocal')}</button>
          <a class="btn primary sm" href="./api/backup" download>${t('bk.dlNow')}</a>
        </div>
      </div>
      <p class="hint">${esc(t('bk.nowHelp'))}</p>
      ${listHTML()}
    </section>
    <div class="bk-pair">
      <section class="card bk-auto" id="bk-auto">${autoHTML()}</section>
      <section class="card bk-restore">
        <div class="card-head"><h2>${t('bk.restore')}</h2></div>
        <p class="hint">${esc(t('bk.restoreHelp'))}</p>
        <label class="bk-drop" tabindex="0" role="button"><input type="file" id="bk-file" accept=".zip,.db" hidden><b>${esc(t('bk.pick'))}</b><span>${esc(t('bk.dropHint'))}</span></label>
      </section>
    </div>
    <details class="card bk-mig"${open ? ' open' : ''}>
      <summary><div><h2>${t('mig.title')}</h2><p class="hint">${esc(t('mig.sum'))}</p></div><i class="i i-chev"></i></summary>
      <p class="hint">${esc(t('mig.help'))}</p>
      ${migHTML()}
    </details>`);
  const pick = el.querySelector('#bk-file');
  if (pick) {
    pick.addEventListener('change', () => { const f = pick.files[0]; pick.value = ''; if (f) restoreUpload(f); }); // 清掉选择:确认框里取消后再选同一个文件也要有反应
    const drop = pick.parentElement; // 整块虚线框都能点,也能把文件拖进来
    drop.addEventListener('keydown', e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); pick.click(); } });
    drop.addEventListener('dragover', e => { e.preventDefault(); drop.classList.add('over'); });
    drop.addEventListener('dragleave', () => drop.classList.remove('over'));
    drop.addEventListener('drop', e => { e.preventDefault(); drop.classList.remove('over'); const f = e.dataTransfer && e.dataTransfer.files[0]; if (f) restoreUpload(f); });
  }
}

export async function render(el) {
  data = await get('backup/list');
  data.files = data.files || [];
  draw(el);
}

export function tick() {}

async function reload() {
  try { data = await get('backup/list'); data.files = data.files || []; draw(); } catch (e) { toast(e.message, 'err'); }
}

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

// 选了一个本机上的备份文件:先让服务器看一眼里面有什么(来源、用户 / 线路 / 上游 / 证书各多少),确认了才还原
async function restoreUpload(f) {
  try {
    const s = await upload('backup/inspect', f);
    openModal(t('bk.confirmTitle') + ' · ' + f.name, summaryHTML(s), async () => {
      await upload('backup/restore', f);
      closeModal();
      waitBack();
    }, { okText: t('bk.restoreBtn'), danger: true });
  } catch (e) { toast(e.message, 'err'); }
}

// 定时备份四项里任何一项改了就存(开关一拨、钟点一选、份数改完离开输入框):存好了才重画这张卡(徽标、置灰跟着变),存不上把控件复原并说明原因
async function saveSched(el) {
  const on = fchk('bk-on'), keep = Math.floor(Number(fv('bk-keep')));
  if (on && !(keep >= 1 && keep <= 365)) { toast(t('bk.keepBad'), 'err'); setHTML('bk-auto', autoHTML()); return; }
  // 关掉时只改钟点(-1):份数和 Telegram 的选择留着,下次打开还是原来那样
  const body = on ? { backupHour: String(Number(fv('bk-hour'))), backupKeep: String(keep), backupTelegram: String(fchk('bk-tg')) } : { backupHour: '-1' };
  if (el) el.disabled = true;
  try {
    await post('settings', body);
    Object.assign(data, { backupHour: body.backupHour }, on ? { backupKeep: body.backupKeep, backupTelegram: body.backupTelegram === 'true' } : {});
    load('settings').catch(() => {});
    toast(t('set.saved'), 'ok');
  } catch (e) { toast(e.message, 'err'); }
  setHTML('bk-auto', autoHTML());
}

registerActions({
  'bk.run': async (_, btn) => {
    btn.disabled = true;
    try { const f = await post('backup/run'); toast(t('bk.saved', { name: f.name }), 'ok'); await reload(); }
    catch (e) { toast(e.message, 'err'); }
    finally { btn.disabled = false; }
  },
  'bk.sched': (_, el) => saveSched(el),
  'bk.del': async name => {
    if (!await confirm(t('common.deleteConfirm', { name }), { danger: true, okText: t('common.delete') })) return;
    try { await del('backup/file?name=' + encodeURIComponent(name)); await reload(); }
    catch (e) { toast(e.message, 'err'); }
  },
  'bk.restoreLocal': async name => {
    try {
      const s = await post('backup/inspect', { name }, SLOW);
      openModal(t('bk.confirmTitle') + ' · ' + name, summaryHTML(s), async () => {
        await post('backup/restore', { name }, SLOW);
        closeModal();
        waitBack();
      }, { okText: t('bk.restoreBtn'), danger: true });
    } catch (e) { toast(e.message, 'err'); }
  },
  'bk.copyCmd': () => copy(migCmd()),
  'bk.dnsCheck': async (_, btn) => {
    const domain = fv('mig-domain').trim(), ip = fv('mig-ip').trim();
    const box = document.getElementById('mig-result');
    if (!domain) { toast(t('cert.needDomain'), 'err'); return; }
    btn.disabled = true;
    box.innerHTML = `<span class="muted small">${t('cert.checking')}</span>`;
    try {
      const p = await post('cert/precheck', { domain }, SLOW);
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
