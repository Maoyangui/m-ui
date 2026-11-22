// 应用入口:登录、壳布局、hash 路由、全局状态与定时刷新。
import { get, post, setUnauthorizedHandler, LONG, abortPageLoads } from './api.js';
import { t, setLang, getLang, langs } from './i18n.js';
import { trErr } from './errmsg.js';
import { ICONS } from './icons.js';
import { toast, confirm, closeModal, closeDrawer, drawerOpen, drawerSeq, esc, setTimezone, fmtDay, registerActions, openSettingsAt } from './ui.js';
import { sortCerts, certSig, sortReach, reachSig, reachHard } from './issues.js';
import { mountXmasEgg } from './xmas.js';
import { mountReward } from './reward.js';
import { attnMuted } from './attnmute.js';
import { navHidden, visibleNav, isConcise, applyConcise } from './uiprefs.js';
import { initQh, setQhLabel, inQh } from './qhelp.js';
import * as dashboard from './pages/dashboard.js';
import * as lines from './pages/lines.js';
import * as upstreams from './pages/upstreams.js';
import * as subnodes from './pages/subnodes.js';
import * as users from './pages/users.js';
import * as plans from './pages/plans.js';
import * as rulesPage from './pages/rules.js';
import * as resellers from './pages/resellers.js';
import * as account from './pages/account.js';
import * as exts from './pages/exts.js';
import * as nodes from './pages/nodes.js';
import * as cert from './pages/cert.js';
import * as backup from './pages/backup.js';
import * as ops from './pages/ops.js';
import * as fw from './pages/fw.js';
import * as logs from './pages/logs.js';
import * as settings from './pages/settings.js';
import * as admin from './pages/admin.js';
import { haNotices } from './ha.js';

export const state = {
  status: {}, settings: {}, lines: [], upstreams: [], users: [], plans: [], nodes: [], exts: [], rules: [], limitStates: [],
  onlines: { users: [], lines: [], upstreams: [], connCounts: {} },
  at: {},  // 各类数据最近一次取成功的时间(毫秒):概况据此判断"面板状态没取到"
  err: {}, // 各类数据最近一次失败的错误(成功后清掉)
};

const pages = { dashboard, lines, subnodes, upstreams, exts, users, plans, rules: rulesPage, resellers, account, nodes, cert, backup, ops, fw, logs, admin, settings };
// 侧栏分三组:网络(流量怎么进、怎么出)、运营(卖给谁、怎么计费)、系统(这台面板自己)。概览单独在最上面
const NAV_GROUPS = [
  { items: ['dashboard'] },
  { key: 'network', items: ['lines', 'subnodes', 'upstreams', 'exts', 'nodes'] },
  { key: 'business', items: ['users', 'resellers', 'plans', 'rules'] },
  { key: 'system', items: ['cert', 'backup', 'ops', 'fw', 'logs', 'admin', 'settings'] },
];
const masterNav = NAV_GROUPS.flatMap(g => g.items);
export const NAV_ORDER = NAV_GROUPS; // 设置 → 界面「侧栏显示的栏目」按这个分组列
// 代理面板:同一套前端,只留下代理用得上的几页
const resellerNav = ['dashboard', 'users', 'plans', 'account'];
export const isReseller = () => (state.status || {}).scope === 'reseller';
const navFor = () => (isReseller() ? resellerNav : masterNav);
const navGroups = () => (isReseller() ? [{ items: resellerNav }] : NAV_GROUPS);
// 侧栏里藏起来的栏目(设置 → 界面,用户 11-21):只是不在侧栏里显示,页面本身照旧(地址直接打开、别处的链接都能到)。代理面板不管
const hiddenNav = () => (isReseller() ? new Set() : navHidden(state.settings));
// homePage 没指定页面(登录后、地址不认得)时去哪:概览;概览藏了就是侧栏里第一个露着的栏目
const homePage = () => (isReseller() ? 'dashboard' : visibleNav(masterNav, state.settings)[0] || 'dashboard');
let current = null;
// routeGen 每换一次页面加一。渲染是异步的,中途换页时旧的那次渲染必须整个作废:
// 它既不该再往页面里写内容,它抛的错也不该显示出来(那正是"点了 A 却看到报错/看到 B"的来源)。
let routeGen = 0;
let routing = false;
// 渲染串行化:页面的 render 内部还会自己 await(账号页就是先取数据再画整页)。
// 两次渲染重叠时,慢的那次会晚一步把内容写进 #page,把新页面顶掉,表现就是"点了 A 却显示 B"。
// 让每次渲染排队等上一次结束,再判断自己是不是还当值,这样最后一次导航一定是赢家。
// 请求本身有超时(见 api.js),渲染不会永远挂住,队列不会卡死。
// 但排队不能变成干等:上一页还没画完就换页时,先把它还在路上的读取放弃掉(abortPageLoads),它马上结束、下一页立刻开始 ——
// 以前上一页有个慢接口(防火墙、运维这类要问各台副机或跑系统命令的),这时点别的页面就一直「加载中」,最久拖到 30 秒超时。
let renderQueue = Promise.resolve();

// ---- 数据加载 ----
// 进哪个页面才拉哪个页面的数据(见下面 PAGE_DATA);拉过的留在 state 里,切换页面不重复请求。
// 页面上的增删改本来就会显式 load('users') 之类刷新自己关心的那份,这一点没有变。
const ALL_DATA = ['status', 'settings', 'lines', 'upstreams', 'users', 'plans', 'nodes', 'exts', 'onlines'];
const loaded = new Set();
export async function ensure(...what) {
  const need = what.filter(k => !loaded.has(k));
  if (need.length) await load(...need);
}
function resetState() {
  loaded.clear();
  Object.assign(state, { status: {}, settings: {}, lines: [], upstreams: [], users: [], plans: [], nodes: [], exts: [], rules: [], limitStates: [],
    onlines: { users: [], lines: [], upstreams: [], connCounts: {} }, at: {}, err: {} });
}
export async function load(...what) {
  const all = what.length === 0;
  const jobs = [];
  // 每类数据各记各的成败时间:概况上"面板状态没取到 · 某时的数据"靠它
  // 换页时被放弃的读取(e.aborted)不算「没取到」:不记进 state.err(概况会拿它报「某某没取到」),也不把空数据当成取到了
  const job = (key, p) => jobs.push(p.then(() => { state.at[key] = Date.now(); delete state.err[key]; }, e => { if (!e || !e.aborted) state.err[key] = e; throw e; }));
  if (all || what.includes('status')) job('status', get('status').then(s => { state.status = s; renderRole(); upgradeNotice(); restoreNotice(); reloadNotice(); reachNotice(); certNotice(); haNotices(s); }));
  if (all || what.includes('settings')) job('settings', get('settings').then(s => { state.settings = s; setTimezone(s.timezone); applyUIPrefs(); }));
  if (all || what.includes('lines')) job('lines', get('lines').then(l => { state.lines = l; }));
  if (!isReseller() && (all || what.includes('upstreams'))) job('upstreams', get('upstreams').then(u => { state.upstreams = u; }));
  if (all || what.includes('users')) job('users', get('users').then(u => { state.users = u; }));
  // 生效中的规则限速:用户列表据此打"限速中"标记(主面板才有;副机 / 代理拿不到就保持空)
  if (!isReseller() && (all || what.includes('users'))) jobs.push(get('limitstates').then(l => { state.limitStates = l || []; }).catch(() => {}));
  if (all || what.includes('plans')) job('plans', get('plans').then(p => { state.plans = p || []; }));
  if (!isReseller() && (all || what.includes('nodes'))) jobs.push(get('nodes').then(n => { state.nodes = (n && n.nodes) || []; }).catch(e => { if (e && e.aborted) throw e; state.nodes = []; }));
  if (!isReseller() && (all || what.includes('exts'))) jobs.push(get('exts').then(x => { state.exts = x || []; }).catch(e => { if (e && e.aborted) throw e; state.exts = []; }));
  if (all || what.includes('onlines')) job('onlines', get('onlines').then(o => { state.onlines = o; }));
  await Promise.all(jobs);
  (all ? ALL_DATA : what).forEach(k => loaded.add(k));
}

// 每个页面真正用到的全局数据;没列的页面只靠 status / settings(登录时就有)
const PAGE_DATA = {
  dashboard: ['onlines'],
  lines: ['lines', 'nodes', 'upstreams', 'onlines'],
  upstreams: ['upstreams', 'lines', 'onlines'],
  subnodes: ['lines', 'nodes'],
  exts: ['exts'],
  users: ['users', 'plans', 'lines', 'exts', 'onlines', 'nodes'],
  plans: ['plans', 'lines', 'nodes'],
  rules: ['users'],
  resellers: ['lines', 'nodes'],
  nodes: ['nodes'],
  fw: ['nodes'],
};

// ---- 登录 ----
let needTotp = false; // 服务端要求过两步验证码后,登录框一直显示验证码栏
function showLogin() {
  document.getElementById('app').hidden = true;
  document.documentElement.lang = getLang() === 'zh' ? 'zh-CN' : 'en';
  const l = document.getElementById('login');
  l.hidden = false;
  document.getElementById('login-title').textContent = t('login.title');
  document.getElementById('login-user-label').textContent = t('login.user');
  document.getElementById('login-pass-label').textContent = t('login.pass');
  document.getElementById('login-submit').textContent = t('login.submit');
  document.getElementById('login-code-label').textContent = t('login.code');
  supportLink(document.getElementById('login-support'));
  helpLink(document.getElementById('login-help'), 'login');
  document.getElementById('login-code-wrap').hidden = !needTotp;
  setTimeout(focusLogin, 50);
}
// 登录页该把光标放在哪:账号没填 → 账号;填了账号没填密码 → 密码;账号密码都有、正在等两步验证码 → 验证码。
// 登录页已经开着时(去别的 App 看验证码再切回来,期间任何一次 401 都会再走到 showLogin)同样按这个顺序,
// 不会把光标从验证码框拉回账号框。
function focusLogin() {
  const u = document.getElementById('lg-user'), p = document.getElementById('lg-pass'), c = document.getElementById('lg-code');
  const target = !u.value ? u : (!p.value ? p : (needTotp ? c : p));
  if (document.activeElement === target) return;
  target.focus();
}
// 401 时回到登录页。代理面板的 401 带 scope: 'reseller'(web/reseller_panel.go):还没登录、没有 status 时,
// 登录页靠它认出这是代理面板,「帮助说明」打开代理视图
setUnauthorizedHandler(d => { if (d && d.scope) panelScope = d.scope; showLogin(); });

document.getElementById('login-form').addEventListener('submit', async ev => {
  ev.preventDefault();
  const err = document.getElementById('lg-err');
  err.textContent = '';
  const btn = document.getElementById('login-submit');
  btn.disabled = true;
  try {
    const body = { username: document.getElementById('lg-user').value, password: document.getElementById('lg-pass').value };
    const code = document.getElementById('lg-code').value.trim();
    if (code) body.code = code;
    await post('login', body);
    document.getElementById('lg-pass').value = '';
    document.getElementById('lg-code').value = '';
    await enterApp();
  } catch (e) {
    const d = e.data || {};
    if (e.status === 401 && d.totp) {
      needTotp = true;
      document.getElementById('login-code-wrap').hidden = false;
      err.textContent = d.needCode ? t('login.codeRequired') : t('login.codeWrong');
      setTimeout(() => document.getElementById('lg-code').focus(), 80);
    } else err.textContent = e.status === 401 ? t('login.failed') : e.message;
  }
  finally { btn.disabled = false; }
});

async function enterApp() {
  document.getElementById('login').hidden = true;
  document.getElementById('app').hidden = false;
  resetState(); // 换账号登录(管理员 ↔ 代理)不能沿用上一个会话拉到的数据
  // 先只拿 status:新代理首次登录时会话是"待设密码"状态,后端只放行 status 与 self/password,
  // 这时去拉 settings 会被 403 拒掉,整个 enterApp 抛错就成了黑屏。
  await load('status');
  if (state.status.mustSetPassword) { // 新代理首次登录:先设密码
    account.forceSetPassword();
    return;
  }
  await load('settings');
  renderChrome();
  route();
}

// "建议 / 支持"按钮:新标签页打开,带上当前语言
function supportLink(a) {
  a.innerHTML = `${ICONS.support || ''}<span class="support-label">${esc(t('support.btn'))}</span>`;
  a.title = t('support.btn');
  a.href = 'support?lang=' + getLang();
}

// "帮助说明"按钮:新标签页打开内嵌的帮助页(help/zh.html、help/en.html,跟面板同一个语言),并定位到当前页面对应的章节。
// 代理面板带上 view=reseller:帮助页只留代理用得上的章节。帮助页和面板其它前端文件一样只在面板路径下提供,用相对地址,
// 改了面板路径也照样打开。键是页面名(login 是登录页),值是帮助页里的章节 id(两种语言一致,web/help_test.go 会核对)。
const HELP_ANCHOR = {
  login: 'first-login', dashboard: 'start', lines: 'lines', subnodes: 'subnodes', upstreams: 'upstreams', exts: 'exts', users: 'users', resellers: 'resellers',
  plans: 'plans', rules: 'rules', nodes: 'nodes', cert: 'cert', backup: 'backup', ops: 'ops', fw: 'fw', logs: 'logs',
  admin: 'security', settings: 'settings',
};
const HELP_ANCHOR_RESELLER = { login: 'rs-login', dashboard: 'rs-start', users: 'users', plans: 'plans', account: 'rs-account' };
const HELP_ICON = '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="9"/><path d="M9.6 9.3a2.5 2.5 0 0 1 4.8.9c0 1.7-2.4 2.2-2.4 3.8"/><path d="M12 17.2h.01"/></svg>';
// panelScope 记住这是哪个面板:登录后取 status.scope;没登录时取接口 401 带回来的(见 setUnauthorizedHandler)。
// 退出登录回到登录页时 status 已经不作数,也还认得出是代理面板
let panelScope = '';
function helpLink(a, page) {
  if (!a) return;
  if ((state.status || {}).scope) panelScope = state.status.scope;
  const rs = panelScope === 'reseller';
  const anchor = (rs ? HELP_ANCHOR_RESELLER : HELP_ANCHOR)[page] || '';
  a.href = `help/${getLang() === 'zh' ? 'zh' : 'en'}.html${rs ? '?view=reseller' : ''}${anchor ? '#' + anchor : ''}`;
  if (a.dataset.lang !== getLang()) { // 文字只在换语言时重画
    a.dataset.lang = getLang();
    a.innerHTML = `${HELP_ICON}<span class="help-label">${esc(t('help.btn'))}</span>`;
    a.title = t('help.btn');
  }
}

// ---- 壳:导航、角色、主题、语言 ----
function renderChrome() {
  document.documentElement.lang = getLang() === 'zh' ? 'zh-CN' : 'en'; // 换成英文时页面语言跟着换(读屏、表头折行规则看它)
  navSig = '';
  renderNav();
  setQhLabel(t('qh.label'));
  document.getElementById('modal-cancel').textContent = t('common.cancel');
  const out = document.getElementById('btn-logout');
  out.innerHTML = ICONS.logout || '';
  out.title = t('common.logout');
  out.setAttribute('aria-label', t('common.logout'));
  applyTheme(localStorage.getItem('m-ui-theme') || 'auto'); // 主题按钮的悬停提示跟着语言换
  supportLink(document.getElementById('btn-support'));
  helpLink(document.getElementById('btn-help'), current || pageName());
  mountReward(document.getElementById('reward-slot')); // 流量奖励的硬币(圣诞树左边),见 reward.js
  mountXmasEgg(document.getElementById('xmas-slot')); // 限时圣诞彩蛋(到 12-31),见 xmas.js
  const sel = document.getElementById('lang-select');
  sel.innerHTML = Object.entries(langs).map(([k, v]) => `<option value="${k}" ${k === getLang() ? 'selected' : ''}>${v}</option>`).join('');
  renderRole();
}
// renderNav 画侧栏:藏起来的栏目不画,一组全藏了连组名一起不画。内容没变就不重画(设置每读一次都会走到这里)
let navSig = '';
function renderNav() {
  const hide = hiddenNav();
  const sig = getLang() + '|' + (isReseller() ? 'r' : 'm') + '|' + [...hide].join(',');
  if (sig === navSig) return;
  navSig = sig;
  document.getElementById('nav').innerHTML = navGroups().map(g => {
    const items = g.items.filter(p => !hide.has(p));
    if (!items.length) return '';
    return (g.key ? `<div class="nav-group">${esc(t('nav.group.' + g.key))}</div>` : '') + items.map(p =>
      `<a href="#/${p}" data-page="${p}"><span class="ic">${ICONS[p] || ''}</span><span class="nav-label">${t('nav.' + p)}</span></a>`).join('');
  }).join('');
  document.querySelectorAll('#nav a').forEach(a => a.classList.toggle('active', a.dataset.page === current)); // 换语言重画后当前页的高亮还在
  reachNotice(); // 导航重画后补上"服务器"的红点
}
// applyUIPrefs 按现在的设置切简洁 / 详细、重画侧栏(设置读回来时、设置页里改了界面那两项时)
export function applyUIPrefs() {
  applyConcise(!isReseller() && isConcise(state.settings));
  if (document.getElementById('nav').childElementCount || navSig) renderNav();
}
function renderRole() {
  const s = state.status || {};
  const b = document.getElementById('role-badge');
  if (s.scope === 'reseller') {
    b.textContent = s.reseller || t('rs.role');
    b.title = s.reseller || '';
    b.className = 'badge warn';
  } else {
    b.textContent = s.role === 'node' ? t('role.node') : t('role.master');
    b.title = '';
    b.className = 'badge ' + (s.role === 'node' ? 'node' : 'primary');
  }
  document.getElementById('sidebar-version').textContent = s.version ? 'v' + s.version : '';
  refreshUpdateBadge();
  // 默认密码未改:顶部常驻警示
  let bar = document.getElementById('default-pw-bar');
  if (s.defaultPassword) {
    if (!bar) {
      bar = document.createElement('div');
      bar.id = 'default-pw-bar';
      bar.className = 'alert-bar';
      document.querySelector('.main').insertBefore(bar, document.getElementById('page'));
    }
    // ?pw:到了管理员页直接弹出修改框(见 pages/admin.js),少点一下
    bar.innerHTML = `<span>${t('alert.defaultPw')}</span><a href="#/admin?pw" class="btn sm">${t('alert.changeNow')}</a>`;
  } else if (bar) bar.remove();
}

// ---- 版本更新 ----
// 面板自己去查有没有新版本(每 6 小时一次,结果缓存在服务端),有就在版本号旁边亮个小箭头。
// 更新只替换二进制再重启服务:数据库、证书、备份、设置都不动,用户与订阅地址不变。
let updateState = null;
let updateCheckedAt = 0;
let updateChecking = false;
async function refreshUpdateBadge() {
  const dot = document.getElementById('update-dot');
  if (!dot || isReseller() || updateChecking) return;
  // 服务端本就缓存 6 小时:成功后半小时再问,失败(副机、网络不通)5 分钟内不重试
  if (Date.now() - updateCheckedAt < (updateState ? 1800000 : 300000)) return;
  updateChecking = true;
  try {
    updateState = await get('update');
  } catch { updateCheckedAt = Date.now(); return; }
  finally { updateChecking = false; }
  updateCheckedAt = Date.now();
  dot.hidden = !updateState.hasUpdate;
  if (updateState.hasUpdate) {
    dot.innerHTML = ICONS.update;
    dot.title = t('update.available', { v: updateState.latest });
  }
}

// updateInfo 版本检查的结果(没查过 / 查不到为 null);promptUpdate 和点版本号旁的箭头一样
export const updateInfo = () => updateState;
export function promptUpdate() { return doUpdate(); }
document.getElementById('update-dot').addEventListener('click', () => doUpdate());
async function doUpdate() {
  if (!updateState) return;
  const ok = await confirm(
    [t('update.confirm', { from: 'v' + updateState.current, to: updateState.latest }), t('update.note')].join(String.fromCharCode(10)),
    { okText: t('update.now') });
  if (!ok) return;
  const dot = document.getElementById('update-dot');
  dot.disabled = true;
  let res;
  try {
    res = await post('update', undefined, LONG);
  } catch (e) {
    dot.disabled = false;
    toast(e.message, 'err');
    return;
  }
  toast(t('update.restarting'), 'ok');
  if (res && res.watch === false) toast(t('update.noWatch'), 'err');
  waitForPanel();
}

// 上次一键更新回滚过:在页面顶部明说,管理员点"知道了"才清掉(状态文件在数据目录里,重启也不会忘)
function upgradeNotice() {
  const st = state.status && state.status.upgrade;
  let bar = document.getElementById('upgrade-bar');
  if (!st) { if (bar) bar.remove(); return; }
  if (bar) return;
  const main = document.querySelector('.main'), page = document.getElementById('page');
  if (!main || !page) return;
  bar = document.createElement('div');
  bar.id = 'upgrade-bar';
  bar.className = 'alert-bar warn';
  bar.innerHTML = `<span><b>${esc(t(st.dbRestored ? 'update.rolledBackDb' : 'update.rolledBack', { from: st.from, to: st.to }))}</b> ${esc(t('update.rollbackHint'))}</span><button class="btn sm" id="upgrade-ack">${esc(t('update.ack'))}</button>`;
  main.insertBefore(bar, page);
  bar.querySelector('#upgrade-ack').addEventListener('click', async () => {
    await post('update/ack').catch(() => {});
    if (state.status) delete state.status.upgrade;
    bar.remove();
  });
}

// 上次从备份还原时,数据目录外的证书放进了数据目录(备份那一刻的副本,没有谁续)、或者还缺 / 没写成证书文件:在页面顶部明说,
// 管理员点"知道了"才清掉。面板上传的还原在重启时应用,这些以前只写进启动日志(结果文件在数据目录里,重启也不会忘)
function restoreNotice() {
  const n = !isReseller() && state.status && state.status.restore;
  let bar = document.getElementById('restore-bar');
  if (!n) { if (bar) bar.remove(); return; }
  if (bar) return;
  const main = document.querySelector('.main'), page = document.getElementById('page');
  if (!main || !page) return;
  const sep = t('reach.sep'), parts = [];
  if ((n.copies || []).length) {
    const list = n.copies.map(c => t('alert.restoreCopy', { name: (c.names || [])[0] || c.from, date: fmtDay(c.notAfter) })).join(sep);
    parts.push(t('alert.restoreCopies', { list }));
  }
  if ((n.missing || []).length) parts.push(t('alert.restoreMissing', { list: n.missing.join(sep) }));
  if ((n.failed || []).length) parts.push(t('alert.restoreFailed', { list: n.failed.join(sep) }));
  bar = document.createElement('div');
  bar.id = 'restore-bar';
  bar.className = 'alert-bar warn';
  bar.innerHTML = `<span><b>${esc(t('alert.restoreTitle'))}</b> ${esc(parts.join(' '))}</span><div class="row"><a href="#/cert" class="btn sm">${esc(t('alert.certView'))}</a><button class="btn sm ghost" id="restore-ack">${esc(t('update.ack'))}</button></div>`;
  main.insertBefore(bar, page);
  bar.querySelector('#restore-ack').addEventListener('click', async () => {
    await post('backup/restore-ack').catch(() => {});
    if (state.status) delete state.status.restore;
    bar.remove();
  });
}

// 最近一次数据面重载失败:保存早已返回"成功",线上其实还是旧配置。常驻在页面顶部,直到下一次重载成功。
function reloadNotice() {
  const rl = state.status && state.status.reload;
  let bar = document.getElementById('reload-bar');
  if (!rl || !rl.at || rl.ok || isReseller() || onDash()) { if (bar) bar.remove(); return; }
  const text = t('alert.reloadFailed', { op: trErr(rl.op || ''), err: trErr(rl.error || '') });
  if (bar && bar.dataset.err === text) return;
  const main = document.querySelector('.main'), page = document.getElementById('page');
  if (!main || !page) return;
  if (!bar) {
    bar = document.createElement('div');
    bar.id = 'reload-bar';
    bar.className = 'alert-bar warn';
    main.insertBefore(bar, page);
  }
  bar.dataset.err = text;
  bar.innerHTML = `<span><b>${esc(text)}</b> ${esc(t('alert.reloadHint'))}</span><button class="btn sm" id="reload-retry">${esc(t('alert.retryReload'))}</button>`;
  bar.querySelector('#reload-retry').addEventListener('click', async (e) => {
    e.target.disabled = true;
    try { await post('reload', undefined, LONG); toast(t('dash.reloaded'), 'ok'); }
    catch (err) { toast(err.message, 'err'); }
    finally { await load('status').catch(() => {}); }
  });
}

// 证书要处理:自动续期在失败、正用着又快到期 / 过期 / 文件读不出来的(主机连副机的一起;副机一般没配通知,
// 它的证书出了问题以前只有主机面板上锁图标后的天数能看出来)。顶部常驻一条,可以按当前这组情况关掉,情况变了再出现。
function certNotice() {
  // 设置「概况提醒」里关掉了证书这一类:顶部这条也不再常驻(证书页照常显示)
  const list = (!isReseller() && !attnMuted(state.settings).has('cert') && state.status && state.status.certAlerts) || [];
  let bar = document.getElementById('cert-bar');
  const sig = certSig(list);
  let dismissed = '';
  try { dismissed = localStorage.getItem('m-ui.certDismissed') || ''; } catch {}
  if (!list.length || dismissed === sig || onDash()) { if (bar) bar.remove(); return; }
  const what = a => {
    if (a.trouble === 'failing') return t('alert.certFailing');
    // 面板 / 订阅 / 代理面板读不出这张证书,正用临时自签证书顶着(换机还原后最常见):说清楚是谁在顶
    if (a.trouble === 'missing' && a.fallback) return t('alert.certFallback', { svc: a.fallback.split(',').map(x => t('alert.svc.' + x)).join(t('reach.sep')) });
    if (a.trouble === 'missing') return t('alert.certMissing');
    const left = Math.floor((a.notAfter - Date.now() / 1000) / 86400);
    if (left < 0) return t('alert.certExpired');
    if (a.restored) return t('alert.certRestored', { n: left }); // 换机还原放进来的副本:没有谁续它
    return t(a.method === 'manual' ? 'alert.certManual' : 'alert.certLeft', { n: left });
  };
  // 最要紧的在前(续期失败 → 文件读不出 → 已过期 → 快到期),只列 3 张,其余写「等 N 张」
  const shown = sortCerts(list).slice(0, 3).map(a => (a.node ? a.node + ' · ' : '') + a.name + ' ' + what(a)).join(t('reach.sep'));
  const text = t('alert.certs', { list: shown + (list.length > 3 ? t('alert.certMore', { n: list.length, more: list.length - 3 }) : '') });
  if (bar && bar.dataset.sig === sig && bar.dataset.text === text) return;
  const main = document.querySelector('.main'), page = document.getElementById('page');
  if (!main || !page) return;
  if (!bar) {
    bar = document.createElement('div');
    bar.id = 'cert-bar';
    main.insertBefore(bar, page);
  }
  bar.className = 'alert-bar' + (list.some(a => a.trouble !== 'expiring') ? '' : ' warn');
  bar.dataset.sig = sig;
  bar.dataset.text = text;
  const where = list.some(a => !a.nodeId) ? 'cert' : 'nodes?f=bad'; // 本机的去证书页,只有副机的去服务器页(只看异常)
  bar.innerHTML = `<span title="${esc(text)}"><b>${esc(text)}</b></span><div class="row"><a href="#/${where}" class="btn sm">${esc(t('alert.certView'))}</a><button class="btn sm ghost" id="cert-dismiss">${esc(t('reach.dismiss'))}</button></div>`;
  bar.querySelector('#cert-dismiss').addEventListener('click', () => {
    try { localStorage.setItem('m-ui.certDismissed', sig); } catch {}
    bar.remove();
  });
}

// 大陆连通异常(被墙 / 部分运营商不通):导航"服务器"上亮红点,顶部常驻一条提示。
// 提示可以点"知道了"按当前这组情况关掉;又有别的服务器出事、或同一台换了结论,再出现。
function reachNotice() {
  const bad = (!isReseller() && !attnMuted(state.settings).has('reach') && state.status && state.status.reachBad) || []; // 关掉了大陆连通这一类:红点和顶部这条都不再提
  const nav = document.querySelector('#nav a[data-page="nodes"]');
  if (nav) nav.classList.toggle('has-alert', bad.length > 0);
  let bar = document.getElementById('reach-bar-top');
  const sig = reachSig(bad);
  let dismissed = '';
  try { dismissed = localStorage.getItem('m-ui.reachDismissed') || ''; } catch {}
  if (!bad.length || dismissed === sig || onDash()) { if (bar) bar.remove(); return; }
  // 不通的在前,只列 3 台,其余写「等 N 台」(40 台出事时以前能把手机首屏占满)
  const list = sortReach(bad).slice(0, 3).map(r => `${r.name} ${t('reach.v.' + r.verdict)}`).join(t('reach.sep')) + (bad.length > 3 ? t('reach.more', { n: bad.length, more: bad.length - 3 }) : '');
  const text = t('reach.alert', { list });
  if (bar && bar.dataset.sig === sig && bar.dataset.text === text) return;
  const main = document.querySelector('.main'), page = document.getElementById('page');
  if (!main || !page) return;
  if (!bar) {
    bar = document.createElement('div');
    bar.id = 'reach-bar-top';
    main.insertBefore(bar, page);
  }
  const blocked = reachHard(bad).length > 0;
  bar.className = 'alert-bar' + (blocked ? '' : ' warn');
  bar.dataset.sig = sig;
  bar.dataset.text = text;
  bar.innerHTML = `<span title="${esc(text)}"><b>${esc(text)}</b></span><div class="row"><a href="#/nodes?f=bad" class="btn sm">${esc(t('reach.view'))}</a><button class="btn sm ghost" id="reach-dismiss">${esc(t('reach.dismiss'))}</button></div>`;
  bar.querySelector('#reach-dismiss').addEventListener('click', () => {
    try { localStorage.setItem('m-ui.reachDismissed', sig); } catch {}
    bar.remove();
  });
}

// 面板重启期间接口会短暂不可用:轮询到恢复就自动刷新页面
async function waitForPanel() {
  for (let i = 0; i < 60; i++) {
    await new Promise(r => setTimeout(r, 2000));
    try {
      const s = await get('status');
      if (s && s.version) { location.reload(); return; }
    } catch { /* 还没起来,继续等 */ }
  }
  toast(t('update.timeout'), 'err');
}

document.getElementById('lang-select').addEventListener('change', e => {
  setLang(e.target.value);
  renderChrome();
  route(true);
});
document.getElementById('btn-logout').addEventListener('click', async () => {
  await post('logout').catch(() => {});
  showLogin();
});
document.getElementById('btn-theme').addEventListener('click', () => {
  const cur = document.documentElement.dataset.theme || 'auto';
  const next = { auto: 'light', light: 'dark', dark: 'auto' }[cur];
  applyTheme(next);
});
function applyTheme(mode) {
  if (mode === 'auto') delete document.documentElement.dataset.theme;
  else document.documentElement.dataset.theme = mode;
  localStorage.setItem('m-ui-theme', mode);
  const b = document.getElementById('btn-theme');
  b.innerHTML = ICONS[{ auto: 'themeAuto', light: 'themeLight', dark: 'themeDark' }[mode]] || '';
  b.title = t('common.theme') + ' · ' + t('theme.' + mode);
}
applyTheme(localStorage.getItem('m-ui-theme') || 'auto');

// 移动端侧栏
const shell = document.getElementById('app');
document.getElementById('btn-menu').addEventListener('click', () => shell.classList.toggle('nav-open'));
document.getElementById('backdrop').addEventListener('click', () => shell.classList.remove('nav-open'));
document.getElementById('nav').addEventListener('click', () => shell.classList.remove('nav-open'));

// 弹窗/抽屉关闭
const modal = document.getElementById('modal');
const cancelModal = () => { closeModal(); modal.dispatchEvent(new Event('modal-cancel')); };
document.getElementById('modal-cancel').addEventListener('click', cancelModal);
modal.addEventListener('click', e => { if (e.target === modal) cancelModal(); });
document.getElementById('drawer-close').addEventListener('click', closeDrawer);
// 点抽屉外面收起抽屉(用户 11-21:「现在支持点击弹窗外的其他区域也会收起」)。几种不算「点外面」:
//   - 按下时抽屉还没开(就是打开它的那一下)、按下在抽屉里(在抽屉里选字拖到外面松手);
//   - 从抽屉里点出来的弹窗开着(弹窗里怎么点都不收抽屉;盖在弹窗上的抽屉除外,点弹窗就是点它外面);
//   - 「?」和它的说明浮层、提示条、页面或表格的滚动条;按下后拖了一段(在页面上选字);
//   - 这一下又打开了一个抽屉(点了另一行的「详情」:换成那一行,不是收起)。
let drawerDown = null;
function outsideDrawer(e) {
  const el = e.target;
  if (!(el instanceof Element) || !drawerOpen()) return false;
  const d = document.getElementById('drawer');
  if (d.contains(el) || inQh(el) || el.closest('#toast')) return false;
  if (!modal.hidden && !d.classList.contains('over')) return false;
  if (el === document.documentElement || el === document.body) return false;
  if (el.clientWidth > 0 && (e.offsetX > el.clientWidth || e.offsetY > el.clientHeight)) return false; // 按在滚动条上
  return true;
}
document.addEventListener('pointerdown', e => { drawerDown = outsideDrawer(e) ? { x: e.clientX, y: e.clientY, seq: drawerSeq() } : null; }, true);
document.addEventListener('click', e => {
  const down = drawerDown;
  drawerDown = null;
  if (!down || !outsideDrawer(e) || Math.hypot(e.clientX - down.x, e.clientY - down.y) > 6) return;
  // 等这一下的处理都跑完再看:点的是另一行的「详情」时抽屉已经换成那一行了,不收
  setTimeout(() => { if (drawerSeq() === down.seq && drawerOpen()) closeDrawer(); }, 0);
});
document.addEventListener('keydown', e => {
  // Esc 关闭弹窗/抽屉;Ctrl/⌘+Enter 提交弹窗;"/" 聚焦当前页搜索框
  if (e.key === 'Escape') {
    // 弹窗里的搜索框(线路选择器、勾选列表、用户选择器)有字时,Esc 只清空它,不关掉整个弹窗、丢掉没保存的修改
    const a = document.activeElement;
    if (a && !modal.hidden && modal.contains(a) && a.matches('input[type=search]') && a.value) {
      e.preventDefault();
      a.value = '';
      a.dispatchEvent(new Event('input', { bubbles: true }));
      return;
    }
    if (drawerOpen() && document.getElementById('drawer').classList.contains('over')) closeDrawer(); // 盖在弹窗上的抽屉先关
    else if (!modal.hidden) cancelModal();
    else if (drawerOpen()) closeDrawer();
    return;
  }
  if ((e.ctrlKey || e.metaKey) && e.key === 'Enter' && !modal.hidden) {
    const save = document.getElementById('modal-save');
    if (!save.hidden && !save.disabled) save.click();
    return;
  }
  if (e.key === '/' && modal.hidden && !/^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement?.tagName || '')) {
    const search = document.querySelector('#page input[type=search]');
    if (search) { e.preventDefault(); search.focus(); search.select(); }
  }
});
// 点击菜单外关闭 <details class="menu">
document.addEventListener('click', e => {
  document.querySelectorAll('details.menu[open]').forEach(d => { if (!d.contains(e.target)) d.removeAttribute('open'); });
});

// ---- 路由 ----
// 概况等页面的"查看全部"链接带筛选参数(#/users?f=online、#/nodes?f=bad):目标页 render 时读一次写进自己的模块变量,
// 再用 takeHashParams 把参数从地址栏去掉,之后手动改的筛选不会被地址覆盖。
export const hashParams = () => new URLSearchParams(location.hash.split('?')[1] || '');
export function takeHashParams(page) {
  const p = hashParams();
  if ([...p.keys()].length) history.replaceState(null, '', '#/' + page);
  return p;
}
// 列表类页面随窗口放宽(最宽 2400px),表单类保持 1600px
const FLUID = new Set(['dashboard', 'lines', 'subnodes', 'upstreams', 'exts', 'users', 'plans', 'resellers', 'rules', 'nodes', 'logs', 'settings', 'fw']);
// 概况页自己有「需要处理」卡:同一份数据的重载失败、证书、大陆连通三条顶部提示条在概况页不再叠在上面
const onDash = () => current === 'dashboard';
function pageName() {
  const h = location.hash.replace(/^#\/?/, '').split(/[/?]/)[0];
  // 代理会话下,主面板专属页面一律回概览(那些接口本来也是 403,别让界面误导人)
  if (!pages[h] || !navFor().includes(h)) return homePage();
  return h;
}
// ---- 表格按「自己能用的宽度」排版 ----
// 列表表格放不下时不是横向滚动、也不是把列挤成一字一行,而是先藏次要的列,再放不下就每行排成一张卡片
// (见 style.css「表格按自己能用的宽度排版」)。按表格所在容器的宽度算,不按窗口宽度:
// 侧栏占不占地方、表格在页面里还是在抽屉 / 弹框里,能用的宽度差得很远。
// 各页面照常渲染表格,要调门槛的在 <table> 上写 data-fit,这里统一量、统一加 class:
//   data-fit="cards:840 p2:960 p3:1160"   容器不到 1160 藏 c-p3 列,不到 960 再藏 c-p2 列,不到 840 排成卡片
//   rows:930                              (服务器表)不到 930 每台排成两行
// 没写的档用默认值:卡片 700(抽屉 / 弹框里的表 480,它们本来就按窄容器设计),p2 960,p3 1180。
const CARD_SKIP = '.kv, .api-doc, .conns'; // 键值表、接口文档、连接列表保持原样
const FIT_DEFAULT = { p2: 960, p3: 1180, rows: 0 };
function fitTables() {
  document.querySelectorAll('table.grid').forEach(tb => {
    if (tb.matches(CARD_SKIP)) return;
    const box = tb.parentElement;
    // 量边框外沿:排成卡片后外框没有边线了,量内宽的话两种排法差 2 像素,正好卡在门槛上会来回跳
    const w = box ? box.getBoundingClientRect().width : 0;
    if (!w) return; // 还没排进页面(藏着的页签、收起的折叠块):等它露出来再量
    const fit = { ...FIT_DEFAULT, cards: tb.closest('#page') ? 700 : 480 };
    for (const m of (tb.dataset.fit || '').matchAll(/([a-z0-9]+):(\d+)/g)) fit[m[1]] = Number(m[2]);
    const cards = w < fit.cards, rows = !cards && w < fit.rows;
    tb.classList.toggle('cards', cards);
    tb.classList.toggle('rows2', rows);
    tb.classList.toggle('no-p3', !cards && w < fit.p3);
    tb.classList.toggle('no-p2', !cards && w < fit.p2);
    // 门槛是按常见内容定的,量的是容器、不是内容:英文比中文宽,个别内容(同时设了两种限速的节点、很长的名字)也会把某一列撑宽,
    // 这时按门槛排完表格还是比容器宽(节点设置页英文 1230 宽时「Settings」按钮被截掉一半)。真放不下的再往下藏一档:先 c-p3,再 c-p2。
    // 每次都从门槛重新算起,结果只跟当时的宽度和内容有关,不会来回跳。
    if (!cards) for (const cls of ['no-p3', 'no-p2']) {
      if (tb.getBoundingClientRect().width <= box.clientWidth + 1) break;
      tb.classList.add(cls);
    }
    // 藏完两档还是比容器宽(名字特别长、英文更宽):排成卡片。以前就这么宽着,框里横向滚,右边的「测试 / 编辑 / 删除」被挤到看不见的地方
    // (上游页从订阅里加来的上游名字常常很长,880–1230 宽时就是这样)。照样每次从门槛算起,结果只跟当时的宽度和内容有关
    if (!cards && tb.getBoundingClientRect().width > box.clientWidth + 1) {
      tb.classList.add('cards');
      tb.classList.remove('rows2', 'no-p3', 'no-p2');
    }
    // 跨整行的格子(组名一行、「没有数据」一行)写的是全部列数。藏掉几列以后还按原数跨,表格就多出几列空的:
    // 定宽布局的表会把宽度分给这些空列,右边空出一截、前面几列被挤窄(服务器页就是这样被挤成一字一行的)。按现在露着的列数改
    const cols = [...tb.querySelectorAll(':scope > thead th')].filter(th => getComputedStyle(th).display !== 'none').length;
    if (cols) tb.querySelectorAll(':scope > tbody > tr > td[colspan]').forEach(td => { if (td.colSpan > 1 && td.colSpan !== cols) td.colSpan = cols; });
  });
}
// ---- 长表的高度:尽量只留一条滚动条 ----
// 带 .fill 的表格框自己限高、在里面滚(表头吸在框顶)。限高原先是按「窗口高 − 顶栏 − 估的一段」算的(见 style.css),估小了
// (演示条、批量条、工具栏折行)框就伸到窗口外面去:表格里一条滚动条、整页又一条 —— 用户 11-17 截图:「尽量避免双滑条」。
// 现在量出来:框的上沿在哪、框下面到页面底还有多少,把框正好撑到窗口底 —— 整页不用滚,只剩表格里那一条。
// 窗口太矮、留给表格的不到几行时反过来:表格不限高,跟着整页滚(也只有一条)。两种情况内容都看得全,不截断。
// 嵌在别的东西里面的(外部节点展开的面板、抽屉 / 弹框里的表)不归这里管:它上下都还有内容,整页本来就要滚,
// 它自己很长时还是得有一条 —— 用户也说了「确实容不下的情况可以出现双滑条」,那里只在 CSS 里把高度上限放宽。
const FILL_MIN = 240; // 留给表格框的高度不到这么多(约五行)就不在里面滚了
function fitFill() {
  const page = document.getElementById('page');
  if (!page) return;
  const vh = window.innerHeight;
  page.querySelectorAll('.table-wrap.fill').forEach(wrap => {
    if (wrap.closest('td') || !wrap.offsetParent) return;
    const tb = wrap.querySelector(':scope > table.grid');
    if (tb && (tb.classList.contains('cards') || tb.classList.contains('rows2'))) { wrap.style.maxHeight = ''; return; } // 卡片 / 两行一台:样式里本来就不限高
    const wr = wrap.getBoundingClientRect(), pr = page.getBoundingClientRect();
    // 框上沿在整页里的位置 + 框下面还有的内容(后面的兄弟、页面的下内边距):这两段和框自己多高无关
    const room = vh - (wr.top + window.scrollY) - Math.max(0, pr.bottom - wr.bottom);
    const want = room >= FILL_MIN ? Math.floor(room) + 'px' : 'none';
    if (wrap.style.maxHeight !== want) wrap.style.maxHeight = want;
  });
}
// 排成卡片时每格前面要有列名:每格补上它那一列的表头(只改属性、不动节点,不会反过来触发自己)。
function labelTables() {
  document.querySelectorAll('table.grid').forEach(tb => {
    if (tb.matches(CARD_SKIP)) return;
    const heads = [...tb.querySelectorAll('thead th')].map(th => th.textContent.trim());
    tb.querySelectorAll('tbody tr').forEach(tr => {
      let col = 0;
      for (const td of tr.children) {
        const span = td.colSpan || 1;
        if (span > 1) td.dataset.span = '';
        else if (heads[col] && td.dataset.label !== heads[col]) td.dataset.label = heads[col];
        col += span;
      }
    });
  });
  fitTables();
  fitFill();
}
let labelQueued = false;
const queueTables = () => {
  if (labelQueued) return;
  labelQueued = true;
  requestAnimationFrame(() => { labelQueued = false; labelTables(); });
};
new MutationObserver(queueTables).observe(document.body, { childList: true, subtree: true });
// 窗口宽度变了(含侧栏收起 / 展开的那个断点)重新量;抽屉 / 弹框打开是节点变化,上面已经管了
window.addEventListener('resize', queueTables);
// 页面内容的高度变了但没有增删节点的(折叠块展开、批量条露出来、图片加载完):表格框的高度要跟着重算
document.addEventListener('toggle', queueTables, true);
if (typeof ResizeObserver === 'function') {
  const ro = new ResizeObserver(queueTables);
  const watch = () => { const p = document.getElementById('page'); if (p) ro.observe(p); else requestAnimationFrame(watch); };
  watch();
}

// 页面有没保存的改动(页面模块导出 leaveGuard,返回要问的那句话)时,换页前先问;不走就把地址改回去、页面原样留着。
let skipRoute = false, guarding = false;
async function route(force = false) {
  if (skipRoute) { skipRoute = false; return; } // 刚把地址改回去的那一次:什么都不用做
  const name = pageName();
  const from = pages[current];
  if (!force && current && name !== current && from && from.leaveGuard) {
    if (guarding) return;
    const msg = from.leaveGuard();
    if (msg) {
      guarding = true;
      const go = await confirm(msg, { okText: t('common.leave'), danger: true });
      guarding = false;
      if (!go) {
        if (pageName() !== current) { skipRoute = true; location.hash = '#/' + current; }
        return;
      }
      if (pageName() !== name) return route(force); // 等确认的时候地址又变了:按最新的来
    }
  }
  if (!force && current === name && pages[name].keepAlive) return;
  const gen = ++routeGen;
  if (routing) abortPageLoads(); // 上一页还没画完:它没回来的读取不要了,别让这一页排在后面干等
  current = name;
  routing = true;
  document.querySelectorAll('#nav a').forEach(a => a.classList.toggle('active', a.dataset.page === name));
  helpLink(document.getElementById('btn-help'), name);
  const page = pages[name];
  document.getElementById('page-title').textContent = page.title();
  const sub = document.getElementById('page-subtitle');
  sub.textContent = sub.title = page.subtitle ? page.subtitle() : ''; // 窄屏只显示两行,全文在悬停提示里
  document.getElementById('topbar-right').innerHTML = '';
  closeDrawer();
  const el = document.getElementById('page');
  el.dataset.layout = FLUID.has(name) ? 'fluid' : '';
  el.dataset.page = name;
  reloadNotice(); certNotice(); reachNotice(); // 进出概况页时三条提示条跟着收起 / 回来
  el.innerHTML = `<div class="empty">${t('common.loading')}</div>`;
  el.__muiHTML = null; // 页面区换成了占位:setHTML 记着的上一页作废(它自己也会按节点认出来,这里再明说一次)
  const prev = renderQueue;
  renderQueue = (async () => {
    await prev.catch(() => {});
    if (gen !== routeGen) return; // 排队期间又换页了,这一次直接不画
    try {
      // 能自己显示"没取到"的页面(概况,导出 tickOnError):预取失败照样画,哪块没取到哪块写明,不整页换成报错卡
      await ensure(...(PAGE_DATA[name] || [])).catch(e => { if (e.status === 401 || !page.tickOnError) throw e; });
      if (gen !== routeGen) return;
      await page.render(el);
    } catch (e) {
      if (gen !== routeGen) return; // 过时渲染抛的错,不能盖掉新页面
      if (e.status !== 401) el.innerHTML = `<div class="card err">${esc(e.message)}</div>`;
    }
  })();
  await renderQueue;
  if (gen === routeGen) routing = false;
  if (gen !== routeGen) return;
  // 副机上线路/上游/用户/套餐/外部节点由主机下发:只读展示,隐藏增删改按钮
  const readOnly = state.status.role === 'node' && ['lines', 'subnodes', 'upstreams', 'users', 'plans', 'exts', 'rules', 'fw'].includes(name);
  el.classList.toggle('node-readonly', readOnly);
  if (readOnly) el.insertAdjacentHTML('afterbegin', `<div class="alert-bar info">${t('app.nodeReadOnly')}</div>`);
  bumpRefresh(); // 换页后立刻取一次新数据:进来看到的不该是上次缓存的旧值
}
window.addEventListener('hashchange', () => route());

// ---- 定时刷新(仅可见时)----
// 节奏:平时 5 秒一轮,和主副机同步的 5 秒对齐(副机那部分数据本来就是 5 秒一更,再快也没有更新的东西);
// 刚操作过或刚换页的十几秒内 2 秒一轮,让连接数、在线状态这类本机数据立刻跟上。
// 上一轮还没回来就跳过这一轮:服务器慢或网络差时,不会把同样的请求一轮轮堆起来。
const REFRESH_SLOW = 5000, REFRESH_FAST = 2000, FAST_WINDOW = 12000;
let refreshing = false;
let fastUntil = 0;
export async function refreshNow() {
  if (refreshing || routing) return false; // 页面正在渲染时不插队,免得往还没画好的 DOM 上写
  // 停在登录页时不刷新:没登录,请求只会换回一个 401,又把登录页重画一遍、把光标从验证码框拉回账号框
  // (切去别的 App 看两步验证码再切回来,visibilitychange 就会走到这里)
  if (document.getElementById('app').hidden) return false;
  refreshing = true;
  const gen = routeGen;
  try {
    let failed = false;
    try { await load('status', 'onlines'); } catch { failed = true; }
    if (gen !== routeGen) return false; // 拉数据期间换了页,这一轮的结果不该画到新页面上
    const page = pages[current];
    // 没取到时,只有自己能显示"没取到"的页面(概况,导出 tickOnError)照样画这一轮
    if (page && page.tick && (!failed || page.tickOnError)) await page.tick();
  } catch {}
  finally { refreshing = false; }
  return true;
}
// bumpRefresh 进入"快刷"窗口并立刻刷一次(换页后、用户操作后)
export function bumpRefresh() {
  fastUntil = Date.now() + FAST_WINDOW;
  refreshNow();
}
document.addEventListener('mui:acted', () => { fastUntil = Date.now() + FAST_WINDOW; });
document.addEventListener('visibilitychange', () => { if (!document.hidden) bumpRefresh(); });
(function refreshLoop() {
  setTimeout(() => {
    if (!document.getElementById('app').hidden && !document.hidden) refreshNow();
    refreshLoop();
  }, Date.now() < fastUntil ? REFRESH_FAST : REFRESH_SLOW);
})();

// 顶栏高度(窄屏下标题会折行)写进 --topbar-h:吸附的批量条、表格限高滚动区、侧栏顶上那一格都按它算。
// 量带小数的实际高度(offsetHeight 取整,写回去会让读它的地方差零点几像素);顶栏自己的最小高度不用这个变量(见 style.css .topbar),
// 不然写回去又改了顶栏自己的高度,高过一次就缩不回去
(() => {
  const bar = document.querySelector('.topbar');
  if (!bar || typeof ResizeObserver === 'undefined') return;
  new ResizeObserver(() => { const h = bar.getBoundingClientRect().height; if (h) document.documentElement.style.setProperty('--topbar-h', h + 'px'); }).observe(bar);
})();

// 链接写 data-act="go.setting" data-id="分区或某一项":跳到设置页的那个位置(见 ui.js openSettingsAt)
registerActions({ 'go.setting': id => openSettingsAt(id) });

// ---- 启动 ----
initQh();
get('status').then(async s => { state.status = s; await enterApp(); upgradeNotice(); }).catch(showLogin);
