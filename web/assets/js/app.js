// 应用入口:登录、壳布局、hash 路由、全局状态与定时刷新。
import { get, post, setUnauthorizedHandler } from './api.js';
import { t, setLang, getLang, langs } from './i18n.js';
import { toast, closeModal, closeDrawer, drawerOpen, esc, setTimezone } from './ui.js';
import * as dashboard from './pages/dashboard.js';
import * as lines from './pages/lines.js';
import * as upstreams from './pages/upstreams.js';
import * as users from './pages/users.js';
import * as plans from './pages/plans.js';
import * as resellers from './pages/resellers.js';
import * as account from './pages/account.js';
import * as exts from './pages/exts.js';
import * as nodes from './pages/nodes.js';
import * as cert from './pages/cert.js';
import * as backup from './pages/backup.js';
import * as ops from './pages/ops.js';
import * as logs from './pages/logs.js';
import * as settings from './pages/settings.js';
import * as admin from './pages/admin.js';

export const state = {
  status: {}, settings: {}, lines: [], upstreams: [], users: [], plans: [], nodes: [], exts: [],
  onlines: { users: [], lines: [], upstreams: [], connCounts: {} },
};

const pages = { dashboard, lines, upstreams, exts, users, plans, resellers, account, nodes, cert, backup, ops, logs, admin, settings };
const masterNav = [
  ['dashboard', '◉'], ['lines', '⇄'], ['upstreams', '⇪'], ['exts', '⇢'], ['users', '☺'], ['resellers', '⚑'], ['plans', '▤'], ['nodes', '☁'],
  ['cert', '⚿'], ['backup', '⛁'], ['ops', '⚒'], ['logs', '≡'], ['admin', '⚉'], ['settings', '⚙'],
];
// 代理面板:同一套前端,只留下代理用得上的几页
const resellerNav = [['dashboard', '◉'], ['users', '☺'], ['plans', '▤'], ['account', '⚉']];
export const isReseller = () => (state.status || {}).scope === 'reseller';
const navFor = () => (isReseller() ? resellerNav : masterNav);
let current = null;

// ---- 数据加载 ----
export async function load(...what) {
  const all = what.length === 0;
  const jobs = [];
  if (all || what.includes('status')) jobs.push(get('status').then(s => { state.status = s; renderRole(); }));
  if (all || what.includes('settings')) jobs.push(get('settings').then(s => { state.settings = s; setTimezone(s.timezone); }));
  if (all || what.includes('lines')) jobs.push(get('lines').then(l => { state.lines = l; }));
  if (!isReseller() && (all || what.includes('upstreams'))) jobs.push(get('upstreams').then(u => { state.upstreams = u; }));
  if (all || what.includes('users')) jobs.push(get('users').then(u => { state.users = u; }));
  if (all || what.includes('plans')) jobs.push(get('plans').then(p => { state.plans = p || []; }));
  if (!isReseller() && (all || what.includes('nodes'))) jobs.push(get('nodes').then(n => { state.nodes = (n && n.nodes) || []; }).catch(() => { state.nodes = []; }));
  if (!isReseller() && (all || what.includes('exts'))) jobs.push(get('exts').then(x => { state.exts = x || []; }).catch(() => { state.exts = []; }));
  if (all || what.includes('onlines')) jobs.push(get('onlines').then(o => { state.onlines = o; }));
  await Promise.all(jobs);
}

// ---- 登录 ----
let needTotp = false; // 服务端要求过两步验证码后,登录框一直显示验证码栏
function showLogin() {
  document.getElementById('app').hidden = true;
  const l = document.getElementById('login');
  l.hidden = false;
  document.getElementById('login-title').textContent = t('login.title');
  document.getElementById('login-user-label').textContent = t('login.user');
  document.getElementById('login-pass-label').textContent = t('login.pass');
  document.getElementById('login-submit').textContent = t('login.submit');
  document.getElementById('login-code-label').textContent = t('login.code');
  supportLink(document.getElementById('login-support'));
  document.getElementById('login-code-wrap').hidden = !needTotp;
  setTimeout(() => document.getElementById('lg-user').focus(), 50);
}
setUnauthorizedHandler(showLogin);

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
  await load('status');
  if (state.status.mustSetPassword) { // 新代理首次登录:先设密码
    account.forceSetPassword();
    return;
  }
  renderChrome();
  await load();
  route();
}

// "建议 / 支持"按钮:新标签页打开,带上当前语言
function supportLink(a) {
  a.textContent = '✦ ' + t('support.btn');
  a.href = 'support?lang=' + getLang();
}

// ---- 壳:导航、角色、主题、语言 ----
function renderChrome() {
  document.getElementById('nav').innerHTML = navFor().map(([p, ic]) =>
    `<a href="#/${p}" data-page="${p}"><span class="ic">${ic}</span>${t('nav.' + p)}</a>`).join('');
  document.getElementById('modal-cancel').textContent = t('common.cancel');
  document.getElementById('btn-logout').textContent = t('common.logout');
  supportLink(document.getElementById('btn-support'));
  const sel = document.getElementById('lang-select');
  sel.innerHTML = Object.entries(langs).map(([k, v]) => `<option value="${k}" ${k === getLang() ? 'selected' : ''}>${v}</option>`).join('');
  renderRole();
}
function renderRole() {
  const s = state.status || {};
  const b = document.getElementById('role-badge');
  if (s.scope === 'reseller') {
    b.textContent = s.reseller || t('rs.role');
    b.className = 'badge warn';
  } else {
    b.textContent = s.role === 'node' ? t('role.node') : t('role.master');
    b.className = 'badge ' + (s.role === 'node' ? 'node' : 'primary');
  }
  document.getElementById('sidebar-version').textContent = s.version ? 'v' + s.version : '';
  // 默认密码未改:顶部常驻警示
  let bar = document.getElementById('default-pw-bar');
  if (s.defaultPassword) {
    if (!bar) {
      bar = document.createElement('div');
      bar.id = 'default-pw-bar';
      bar.className = 'alert-bar';
      document.querySelector('.main').insertBefore(bar, document.getElementById('page'));
    }
    bar.innerHTML = `<span>${t('alert.defaultPw')}</span><a href="#/admin" class="btn sm">${t('alert.changeNow')}</a>`;
  } else if (bar) bar.remove();
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
  document.getElementById('btn-theme').textContent = { auto: '◐', light: '☀', dark: '☾' }[mode];
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
document.addEventListener('keydown', e => {
  // Esc 关闭弹窗/抽屉;Ctrl/⌘+Enter 提交弹窗;"/" 聚焦当前页搜索框
  if (e.key === 'Escape') {
    if (!modal.hidden) cancelModal();
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
function pageName() {
  const h = location.hash.replace(/^#\/?/, '').split('/')[0];
  // 代理会话下,主面板专属页面一律回概览(那些接口本来也是 403,别让界面误导人)
  if (!pages[h] || !navFor().some(([n]) => n === h)) return 'dashboard';
  return h;
}
async function route(force = false) {
  const name = pageName();
  if (!force && current === name && pages[name].keepAlive) return;
  current = name;
  document.querySelectorAll('#nav a').forEach(a => a.classList.toggle('active', a.dataset.page === name));
  const page = pages[name];
  document.getElementById('page-title').textContent = page.title();
  document.getElementById('page-subtitle').textContent = page.subtitle ? page.subtitle() : '';
  document.getElementById('topbar-right').innerHTML = '';
  closeDrawer();
  const el = document.getElementById('page');
  el.innerHTML = `<div class="empty">${t('common.loading')}</div>`;
  try { await page.render(el); }
  catch (e) { if (e.status !== 401) el.innerHTML = `<div class="card err">${esc(e.message)}</div>`; }
  // 副机上线路/上游/用户/套餐/外部节点由主机下发:只读展示,隐藏增删改按钮
  const readOnly = state.status.role === 'node' && ['lines', 'upstreams', 'users', 'plans', 'exts'].includes(name);
  el.classList.toggle('node-readonly', readOnly);
  if (readOnly) el.insertAdjacentHTML('afterbegin', `<div class="alert-bar info">${t('app.nodeReadOnly')}</div>`);
}
window.addEventListener('hashchange', () => route());

// ---- 定时刷新(仅可见时)----
setInterval(async () => {
  if (document.getElementById('app').hidden || document.hidden) return;
  try {
    await load('status', 'onlines');
    const page = pages[current];
    if (page && page.tick) page.tick();
  } catch {}
}, 10000);

// ---- 启动 ----
get('status').then(async s => { state.status = s; await enterApp(); }).catch(showLogin);
