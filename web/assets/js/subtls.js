// 订阅 http ↔ https:订阅端口改用 / 不再用 HTTPS 时,所有用户手里的订阅地址都会变(scheme 变了),旧地址失效,
// 每个客户端都要重新导入。证书页据此决定「用于订阅 HTTPS」默不默认勾、什么时候要先确认一次。
// 纯函数,不碰 DOM:web/jstest/subtls.test.mjs 直接测。
// data 是 GET /cert 的结果:subCert = 订阅现在引用的证书路径(空 = http),subUsers = 用户数(含停用的、代理名下的)。

const users = data => Number(data?.subUsers) || 0;

// subDefaultOn 添加证书时「用于订阅 HTTPS」默认勾不勾:订阅已经是 https、或者是自签证书时不勾;
// 还没有用户时勾(这时改成 https 谁的地址都不受影响),已有用户时不勾,要管理员自己决定。
export function subDefaultOn(data, selfSign) {
  return !selfSign && !data?.subCert && users(data) === 0;
}

// subSchemeChange 这次操作会不会让所有用户的订阅地址变:会就返回 { from, to, n },不会返回 null。
// next 为布尔:添加证书时勾没勾「用于订阅 HTTPS」(勾了只会 http → https);
// next 为字符串:在「正在使用」里给订阅选的证书名(空 = 不用 HTTPS)。
export function subSchemeChange(data, next) {
  const n = users(data);
  if (n === 0) return null;
  const now = !!data?.subCert;
  const after = typeof next === 'string' ? next !== '' : (next ? true : now);
  if (now === after) return null;
  return { from: now ? 'https' : 'http', to: after ? 'https' : 'http', n };
}

// 订阅服务的判法(sub/server.go):证书和私钥路径都填了才走 HTTPS,缺一个就是 http。
const subHttps = v => !!String(v?.subCertFile ?? '').trim() && !!String(v?.subKeyFile ?? '').trim();

// subSettingsChange 设置页「订阅」组直接改 subCertFile / subKeyFile:before 是现在的设置,after 是要保存的值,
// n 是用户数。http ↔ https 一换就返回 { from, to, n },只是换了路径(scheme 不变)或没有用户返回 null。
export function subSettingsChange(before, after, n) {
  return subSchemeChange({ subCert: subHttps(before) ? 'on' : '', subUsers: n }, subHttps(after) ? 'on' : '');
}
