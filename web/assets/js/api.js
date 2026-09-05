// 面板 API 客户端。所有请求同源相对路径,兼容任意面板路径前缀。
import { t } from './i18n.js';

const BASE = './api/';

export class ApiError extends Error {
  constructor(status, message) { super(message); this.status = status; }
}

let onUnauthorized = () => {};
export function setUnauthorizedHandler(fn) { onUnauthorized = fn; }

// 普通接口 30 秒没回来就当失败(副机"测试"要等主机那边最多 25 秒,所以不能更短);
// 更新、签证书、装 WARP 这类长活由调用方自己传更长的 timeout,传 0 表示不限。
const DEFAULT_TIMEOUT = 30000;

// 传输层可替换:官网的静态 Live Demo 用同一套前端,只把这里换成内存里的演示数据,
// 不发任何网络请求。生产环境从不调用 setTransport,这段对它没有影响。
let transport = null;
export function setTransport(fn) { transport = fn; }
let qrUrlFn = null;
export function setQrUrl(fn) { qrUrlFn = fn; }

export async function api(path, opts = {}) {
  if (transport) return transport(path, opts);
  const { timeout = DEFAULT_TIMEOUT, ...init } = opts;
  const ctl = timeout > 0 ? new AbortController() : null;
  const timer = ctl ? setTimeout(() => ctl.abort(), timeout) : null;
  let res;
  try {
    res = await fetch(BASE + path, {
      headers: { 'Content-Type': 'application/json' },
      ...init,
      signal: ctl ? ctl.signal : undefined,
    });
  } catch (e) {
    if (e && e.name === 'AbortError') throw new ApiError(0, t('api.timeout'));
    throw e;
  } finally {
    if (timer) clearTimeout(timer);
  }
  const data = await res.json().catch(() => ({}));
  if (res.status === 401) { onUnauthorized(); const e = new ApiError(401, data.error || '未登录'); e.data = data; throw e; }
  if (!res.ok) throw new ApiError(res.status, data.error || ('请求失败 ' + res.status));
  return data;
}

export const get = (path, opts) => api(path, opts);
export const post = (path, body, opts) => api(path, { method: 'POST', body: body === undefined ? undefined : JSON.stringify(body), ...opts });
export const put = (path, body, opts) => api(path, { method: 'PUT', body: JSON.stringify(body), ...opts });
export const del = (path, opts) => api(path, { method: 'DELETE', ...opts });
// 长活的预算:在线更新要下载几十 MB 再重启,一次测完所有上游要挨个等超时
export const LONG = { timeout: 600000 };
// 中等长度:签证书前的预检、单个上游测试、刷新外部订阅、副机测试/推送(主机侧最多等 25 秒)
export const SLOW = { timeout: 90000 };

// 上传文件(multipart),extra 为附加字段
export async function upload(path, file, extra = {}) {
  const fd = new FormData();
  fd.append('file', file, file.name);
  Object.entries(extra).forEach(([k, v]) => fd.append(k, v));
  const res = await fetch(BASE + path, { method: 'POST', body: fd });
  if (res.status === 401) { onUnauthorized(); throw new ApiError(401, '未登录'); }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new ApiError(res.status, data.error || ('请求失败 ' + res.status));
  return data;
}

// 二进制资源(二维码)直接返回 URL,由 <img> 加载
export const qrUrl = (userId, format) => qrUrlFn ? qrUrlFn(userId, format) : `${BASE}users/${userId}/qr?format=${format}&_=${Date.now()}`;
