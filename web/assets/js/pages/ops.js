// 运维页:只管「这台服务器自己」。三层:
//   1. 最上面一排小卡(和概览同一种):主机、已运行、内存、Swap、磁盘 —— 一眼看这台机器好不好;用量各带一条细进度条,磁盘紧了变色。
//   2. Cloudflare WARP 和任务输出两张等高的卡并排(用户看过后定的:这两样要在系统优化的上面):WARP 只列现在用得上的信息和按钮
//      (没装只有「安装」,装了没启用是「启用」「卸载」,启用着才有「检测出口」「添加为上游」「停用」),端口就写在 SOCKS5 那一行里改;
//      任务输出没跑过任务时只是一行字,跑起来实时显示。
//   3. 系统优化 —— 一张表,一行一项,列对齐:项目(前面一个圆点:绿勾 = 做过了,空心 = 没做)、现在的值、参数、执行。
//      表头右边是「一键优化」(做前四项)。内核参数那一大段文本平时不摊开,点「修改参数」在弹框里改。
// 排版上走过的弯路:① 最早是六个大小不一的灰色方块(三个并排的小方块 + 三个通栏的),内核参数的大文本框一直摊在页面上;
// ② 改成通栏清单后每一行左边是说明、右边隔着大半屏才是输入框和按钮,右侧参差不齐,上面三个用量方块又宽又空 —— 用户看了说「排版不够好」。
import { get, post, SLOW } from '../api.js';
import { t } from '../i18n.js';
import { esc, fmtBytes, fmtDuration, toast, confirm, openModal, registerActions, badge, fv, diskBrief, diskHogs } from '../ui.js';
import { diskLevel } from '../issues.js';

// tx 有译文用译文,没有(中文界面留空的键)用后端原文
const tx = (k, fb) => { const v = t(k); return v && v !== k ? v : fb; };

export const title = () => t('ops.title');
export const subtitle = () => t('ops.subtitle');
let data = null, pollTimer = null;
let sysctlDraft = null; // 在弹框里改过、还没执行成功的内核参数(执行时用它;没改过用服务器存着的那份)

const TUNE_ALL = ['swap', 'limits', 'ntp', 'sysctl']; // 「一键优化」做的四项(和后端 tune-all 一致)
// 小卡里的运行时间只写最大的两级(37 天 4 小时 / 5 小时 12 分),完整的放悬停提示
const upShort = sec => {
  sec = Number(sec) || 0;
  const d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60);
  return d ? `${d} ${t('time.d')} ${h} ${t('time.h')}` : h ? `${h} ${t('time.h')} ${m} ${t('time.m')}` : `${m} ${t('time.m')}`;
};

export async function render(el, fresh = false) {
  data = await get(fresh ? 'ops?fresh=1' : 'ops'); // 系统信息服务端缓存 30 秒;点刷新或任务跑完才重新采集
  const i = data.info, w = i.warp, st = data.status, p = data.params || { swapGb: 2, noFile: 1048576, sysctl: '', defaultSysctl: '', journalMb: 200, journalDays: 14 };
  const jr = i.journal || { usedMb: 0, limited: false };
  // 后端版本里没有某条任务时给个空壳,免得取 .title 直接把整页打挂
  const task = n => { const b = data.tasks.find(x => x.name === n) || { name: n, title: n, desc: '' }; return { ...b, title: tx('ops.task.' + n + '.title', b.title), desc: tx('ops.task.' + n + '.desc', b.desc) }; };
  const dk = data.disk && data.disk.total ? data.disk : null, dlv = diskLevel(dk);
  const linux = i.linux;
  const dis = linux && i.root && !st.running ? '' : 'disabled'; // 不是 Linux、不是 root、或者已经有任务在跑:执行按钮都置灰
  const runBtn = (n, cls = '', label = t('common.run')) => `<button type="button" class="btn sm${cls}" data-act="ops.run" data-id="${n}" ${dis}>${esc(label)}</button>`;

  // ---- 1. 一排状态小卡 ----
  const pct = (used, total) => (total ? Math.round(used * 100 / total) : 0);
  const stat = (k, v, s, { tone = '', bar = null, extra = '' } = {}) => `<div class="stat${tone}"><div class="k">${esc(k)}</div>${extra}<div class="v">${v}</div><div class="s">${s}</div>
      ${bar == null ? '' : `<div class="progress${bar >= 90 ? ' danger' : bar >= 80 ? ' warn' : ''}"><div class="bar" style="width:${Math.min(100, Math.max(0, bar))}%"></div></div>`}</div>`;
  const usage = (k, used, total, tone = '') => stat(k, total ? pct(used, total) + '%' : esc(t('ops.noSwap')), total ? `${esc(fmtBytes(used, 1))} / ${esc(fmtBytes(total, 1))}` : '', { tone, bar: total ? pct(used, total) : 0 });
  const stats = `<div class="stats ops-stats">
      ${stat(t('ops.host'), `<span class="mono" title="${esc(i.hostname)}">${esc(i.hostname)}</span>`, esc(`${i.os} ${i.kernel || ''} ${i.arch}`.replace(/\s+/g, ' ').trim()),
        { extra: `<button type="button" class="btn sm icon ghost ops-refresh" data-act="ops.refresh" title="${esc(t('common.refresh'))}" aria-label="${esc(t('common.refresh'))}"><i class="i i-refresh"></i></button>` })}
      ${linux ? stat(t('ops.uptime'), `<span title="${esc(fmtDuration(i.uptime))}">${esc(upShort(i.uptime))}</span>`, 'load ' + esc(i.load)) : ''}
      ${linux ? usage(t('ops.mem'), i.memTotal - i.memAvail, i.memTotal) : ''}
      ${linux ? usage(t('ops.swap'), i.swapTotal - i.swapFree, i.swapTotal, i.swapTotal ? '' : ' warn') : ''}
      ${linux ? usage(t('ops.disk'), i.diskTotal - i.diskFree, i.diskTotal, dlv === 'bad' ? ' bad' : dlv === 'warn' ? ' warn' : '') : ''}
    </div>`;

  // ---- 2. 系统优化:一张表,一行一项 ----
  const done = { swap: i.swapTotal > 0, limits: !!i.limits, ntp: i.ntp === 'yes', sysctl: !!(i.tuned && i.cc === 'bbr'), journal: !!jr.limited };
  const cur = {
    swap: i.swapTotal ? fmtBytes(i.swapTotal, 0) : t('ops.noSwap'), limits: i.nofile ? String(i.nofile) : '—', ntp: i.ntp === 'yes' ? t('ops.synced') : i.ntp === 'no' ? t('ops.unsynced') : '—',
    sysctl: `${i.cc || '?'} / ${i.qdisc || '?'}`, journal: jr.usedMb ? jr.usedMb + ' MB' : '—', 'disk-clean': dk ? diskBrief(dk) : '—',
  };
  // 一个带前后缀的小数字框:「上限 [200] MB」「[2] GB」
  const numIn = (id, v, attrs, { pre = '', post = '', label = '' } = {}) => `<label class="ops-num">${pre ? `<span>${esc(pre)}</span>` : ''}<input id="${id}" type="number" inputmode="numeric" value="${esc(v)}" ${attrs}${label ? ` aria-label="${esc(label)}"` : ''}>${post ? `<span>${esc(post)}</span>` : ''}</label>`;
  const row = (n, params) => {
    const tk = task(n), isDisk = n === 'disk-clean';
    const mark = !linux ? '<span class="ops-st"></span>'
      : isDisk ? `<span class="ops-st info${dlv === 'bad' ? ' bad' : dlv === 'warn' ? ' warn' : ''}"></span>`
        : `<span class="ops-st${done[n] ? ' ok' : ''}" title="${esc(t(done[n] ? 'ops.done' : 'ops.todo'))}">${done[n] ? '<i class="i i-check"></i>' : ''}</span>`;
    return `<tr>
      <td class="primary-cell"><div class="ops-name">${mark}<div><b>${esc(tk.title)}</b><div class="sub-cell">${esc(tk.desc)}</div></div></div></td>
      <td class="ops-cur">${linux ? esc(cur[n]) : '—'}${isDisk && dk && diskHogs(dk) ? `<div class="sub-cell">${esc(diskHogs(dk))}</div>` : ''}</td>
      <td><div class="ops-ctl">${params || '<span class="muted">—</span>'}</div></td>
      <td class="actions">${runBtn(n)}</td></tr>`;
  };
  const doneN = TUNE_ALL.filter(n => done[n]).length + (done.journal ? 1 : 0);
  const sysctlEdited = sysctlDraft != null && sysctlDraft !== p.sysctl;
  const tuneCard = `<section class="card">
      <div class="card-head"><h2>${t('ops.tune')}${linux ? `<span class="h-count">${esc(t('ops.tuneCount', { n: doneN, m: TUNE_ALL.length + 1 }))}</span>` : ''}</h2>
        ${runBtn('tune-all', TUNE_ALL.every(n => done[n]) ? '' : ' primary', t('ops.t.tune-all'))}</div>
      <p class="hint xp">${esc(t('ops.tuneHelp'))}</p>
      <div class="table-wrap"><table class="grid ops-table" data-fit="cards:780">
        <thead><tr><th>${t('ops.col.item')}</th><th>${t('ops.col.now')}</th><th>${t('ops.col.param')}</th><th></th></tr></thead>
        <tbody>
          ${row('swap', numIn('ops-swap', p.swapGb, 'min="1" max="64" class="w5"', { post: 'GB', label: task('swap').title }))}
          ${row('limits', numIn('ops-nofile', p.noFile, 'min="1024" max="4194304" step="1024" class="w8"', { label: task('limits').title }))}
          ${row('ntp', '')}
          ${row('sysctl', `<button type="button" class="btn sm" data-act="ops.sysctlEdit">${esc(t('ops.sysctlEdit'))}</button>${sysctlEdited ? `<span class="warn-text small">${esc(t('ops.sysctlDirty'))}</span>` : ''}`)}
          ${row('journal', numIn('ops-jmb', p.journalMb, 'min="20" max="20480" step="10" class="w5"', { pre: t('ops.j.limit'), post: 'MB' }) + numIn('ops-jdays', p.journalDays, 'min="1" max="3650" class="w5"', { pre: t('ops.j.keep'), post: t('ops.j.days') }))}
          ${row('disk-clean', dk && dk.opsAgent ? `<label class="ops-chk"><input type="checkbox" id="ops-opsagent"><span>${esc(t('node.dc.opsAgent'))}</span></label>` : '')}
        </tbody>
      </table></div>
    </section>`;

  // ---- 3. WARP:按现在的状态给信息和按钮 ----
  const exitOk = w.exit === 'on' || w.exit === 'plus';
  const wState = !w.installed ? badge(t('ops.warpNone'), '') : !w.listening ? badge(t('ops.warpOff'), 'warn') : exitOk ? badge(t('ops.warpOn') + ' · warp=' + w.exit, 'ok') : badge(t('ops.warpNoExit'), 'warn');
  const rows = [];
  if (w.listening) rows.push([t('ops.exit'), `<span class="mono">${esc(w.exitIp || '—')}</span> ${esc([w.exitLoc, w.exitColo].filter(Boolean).join(' '))}<button type="button" class="btn sm ops-inline" data-act="ops.warpCheck">${t('ops.checkExit')}</button>`]);
  rows.push([t('ops.warpPort'), `<span class="ops-port"><span class="mono muted">127.0.0.1:</span><input id="ops-port" type="number" inputmode="numeric" min="1" max="65535" value="${esc(data.warpPort)}" aria-label="${esc(t('ops.warpPort'))}"></span><div class="sub-cell">${esc(t('ops.warpPortHelp'))}</div>`]);
  if (w.installed) rows.push([t('ops.services'), `<span class="mono">warp-svc ${esc(w.service || '—')} · warp-socks5 ${esc(w.socks || '—')}</span>${w.status ? ` <span class="muted small">${esc(w.status)}</span>` : ''}`]);
  if (w.listening) rows.push([t('ops.upstream'), data.warpUpstream ? `${badge('warp', 'ok')} <span class="muted small">${esc(t('ops.upstreamHas'))}</span>` : `<button type="button" class="btn sm" data-act="ops.warpUpstream">${t('ops.addUpstream')}</button>`]);
  const wActs = !w.installed ? runBtn('warp-install', ' primary', task('warp-install').title)
    : !w.listening ? runBtn('warp-enable', ' primary', t('ops.t.warp-enable')) + runBtn('warp-uninstall', ' danger end', t('ops.t.warp-uninstall'))
    : runBtn('warp-enable', '', t('ops.warpReenable')) + runBtn('warp-disable', '', t('ops.t.warp-disable')) + runBtn('warp-uninstall', ' danger end', t('ops.t.warp-uninstall'));
  const warpCard = `<section class="card ops-warp">
      <div class="card-head"><h2>Cloudflare WARP</h2>${wState}</div>
      <p class="hint xp">${esc(t('ops.warpIntro'))}</p>
      <dl class="kv">${rows.map(([k, v]) => `<dt>${esc(k)}</dt><dd>${v}</dd>`).join('')}</dl>
      <div class="ops-warp-foot">${wActs}</div>
    </section>`;

  // ---- 4. 任务输出 ----
  const log = (st.log || []).join('\n');
  const outState = st.running ? badge(t('ops.running', { task: tx('ops.task.' + st.current + '.title', st.current) }), 'primary') + `<button type="button" class="btn sm danger" data-act="ops.cancel">${t('common.cancel')}</button>`
    : (st.last && st.last.name ? badge((st.last.ok ? '✓ ' : '✗ ') + tx('ops.task.' + st.last.name + '.title', st.last.name), st.last.ok ? 'ok' : 'danger') : '');
  const outCard = `<section class="card ops-out" id="ops-out">
      <div class="card-head"><h2>${t('ops.log')}</h2><div class="head-acts">${outState}</div></div>
      ${log || st.running ? `<pre class="log" id="ops-log">${esc(log)}</pre>` : `<div class="ops-idle">${esc(t('ops.logIdle'))}</div>`}
    </section>`;

  el.innerHTML = `
    ${!linux ? `<div class="alert-bar warn ops-warn"><span>${esc(t('ops.linuxOnly'))}</span></div>` : (!i.root ? `<div class="alert-bar warn ops-warn"><span>${esc(t('ops.rootOnly'))}</span></div>` : '')}
    ${stats}<div class="ops-pair">${warpCard}${outCard}</div>${tuneCard}`;
  const lg = document.getElementById('ops-log');
  if (lg) lg.scrollTop = lg.scrollHeight;
  if (st.running) startPoll();
}

export function tick() {}

// 上一次进度回来了再约下一次:setInterval 不等返回,弱网下请求叠起来,结束时还会弹两次提示、重绘两遍
let pollGen = 0;
function startPoll() {
  stopPoll();
  const gen = ++pollGen;
  const step = async () => {
    const st = await get('ops/status').catch(() => null);
    if (gen !== pollGen) return; // 这期间被停了
    if (st) {
      const log = document.getElementById('ops-log');
      if (log) { log.textContent = (st.log || []).join('\n'); log.scrollTop = log.scrollHeight; }
      if (!st.running) {
        stopPoll();
        toast(st.last && st.last.ok ? t('ops.taskDone') : t('ops.taskFailed'), st.last && st.last.ok ? 'ok' : 'err');
        if (st.last && st.last.ok) sysctlDraft = null; // 跑成功了:参数已经存下,回到存着的那份
        if (location.hash.startsWith('#/ops')) render(document.getElementById('page'), true);
        return;
      }
    }
    pollTimer = setTimeout(step, 1500);
  };
  pollTimer = setTimeout(step, 1500);
}
function stopPoll() { pollGen++; if (pollTimer) clearTimeout(pollTimer); pollTimer = null; }

registerActions({
  'ops.refresh': () => render(document.getElementById('page'), true),
  'ops.run': async (task, btn) => {
    const t2 = data.tasks.find(x => x.name === task);
    if (!await confirm(t('ops.runConfirm', { task: t2 ? tx('ops.task.' + task + '.title', t2.title) : task }), { danger: !!(t2 && t2.danger) })) return;
    btn.disabled = true;
    try {
      await post('ops/run', {
        task, port: Number(fv('ops-port')) || 0, swapGb: Number(fv('ops-swap')) || 0,
        noFile: Number(fv('ops-nofile')) || 0, sysctl: (task === 'sysctl' || task === 'tune-all') ? (sysctlDraft != null ? sysctlDraft : (data.params || {}).sysctl || '') : '',
        journalMb: Number(fv('ops-jmb')) || 0, journalDays: Number(fv('ops-jdays')) || 0,
        opsAgent: task === 'disk-clean' && !!(document.getElementById('ops-opsagent') || {}).checked,
      });
      toast(t('ops.started'), 'ok');
      await render(document.getElementById('page'));
      startPoll();
      const out = document.getElementById('ops-out'); // 输出在页面最下面:任务一开始就滚过去,不用自己找
      if (out) out.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
    } catch (e) { toast(e.message, 'err'); btn.disabled = false; }
  },
  'ops.cancel': async () => { await post('ops/cancel').catch(() => {}); },
  // 内核参数在弹框里改:「确定」只是记下这份(表里标出「改过,还没执行」),点那一行的「执行」或「一键优化」才写进系统
  'ops.sysctlEdit': () => {
    const p = data.params || {};
    openModal(t('ops.sysctlTitle'), `<p class="hint ops-sysctl-hint xp">${esc(t('ops.sysctlHelp'))}</p>
      <textarea id="ops-sysctl" class="mono ops-sysctl" spellcheck="false" aria-label="${esc(t('ops.sysctlTitle'))}">${esc(sysctlDraft != null ? sysctlDraft : p.sysctl || '')}</textarea>
      <div class="row"><button type="button" class="btn sm" data-act="ops.sysctlDefault">${t('ops.restoreDefault')}</button></div>`, async () => {
      const v = fv('ops-sysctl');
      sysctlDraft = v === (p.sysctl || '') ? null : v;
      render(document.getElementById('page'));
    }, { wide: true, okText: t('common.ok') });
  },
  'ops.sysctlDefault': () => { const ta = document.getElementById('ops-sysctl'); if (ta) ta.value = (data.params || {}).defaultSysctl || ''; },
  'ops.warpCheck': async (_, btn) => {
    btn.disabled = true;
    try { const r = await post('ops/warp-check', undefined, SLOW); toast(`warp=${r.exit} ${r.ip || ''} ${r.loc || ''} ${r.colo || ''}`, r.exit === 'on' || r.exit === 'plus' ? 'ok' : 'err'); }
    catch (e) { toast(e.message, 'err'); }
    finally { btn.disabled = false; }
  },
  'ops.warpUpstream': async (_, btn) => {
    btn.disabled = true;
    try { await post('ops/warp-upstream'); toast(t('ops.upstreamAdded'), 'ok'); render(document.getElementById('page'), true); }
    catch (e) { toast(e.message, 'err'); btn.disabled = false; }
  },
});
