// 首次安装的指引(概览「开始使用」三步)现在是什么状态。概览据此画卡片;顶栏的奖励气泡也看它 —— 指引开着时先不冒。
// 用户 11-23:「旨在为新用户简单明了的运行可用的代理线路 其他的复杂功能让自行探索 可以跳过指引或者设置为副机后没有指引」;
// 「设域名不是必选项 因为有的没有域名会采用自签」。所以只留三步,每一步都能直接点着做,做完一条线路就能用:
//   改默认密码 → 一键建线路(默认 VLESS + Reality:不用证书、不用域名)→ 建用户、发订阅。
// 点「跳过」存进面板设置(guideSkipped),换浏览器、换电脑都不再出;以前在这台浏览器里点过「不再显示」的也算跳过。
import { state } from './app.js';

const ls = k => { try { return localStorage.getItem(k); } catch { return null; } };

export function guideSteps() {
  const s = state.status || {};
  return [
    { ok: !s.defaultPassword, href: '#/admin?pw', key: 'pw' },
    { ok: (s.lines || 0) > 0, href: '#/lines?new=reality', key: 'line' },
    { ok: (s.users || 0) > 0, href: '#/users?new=1', key: 'user' },
  ];
}

export const guideSkipped = () => String((state.settings || {}).guideSkipped) === 'true' || ls('m-ui.hideQuickStart') === '1';

// guideOpen 「开始使用」此刻该不该出:主面板、没跳过、三步还没做完(副机、代理面板都不出)
export function guideOpen() {
  const s = state.status || {};
  if (s.role === 'node' || s.scope === 'reseller') return false;
  return !guideSkipped() && guideSteps().some(x => !x.ok);
}
