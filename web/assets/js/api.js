// 面板 API 客户端。所有请求同源相对路径,兼容任意面板路径前缀。
const BASE = './api/';

export class ApiError extends Error {
  constructor(status, message) { super(message); this.status = status; }
}

let onUnauthorized = () => {};
export function setUnauthorizedHandler(fn) { onUnauthorized = fn; }

export async function api(path, opts = {}) {
  const res = await fetch(BASE + path, {
    headers: { 'Content-Type': 'application/json' },
    ...opts,
  });
  if (res.status === 401) { onUnauthorized(); throw new ApiError(401, '未登录'); }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new ApiError(res.status, data.error || ('请求失败 ' + res.status));
  return data;
}

export const get = path => api(path);
export const post = (path, body) => api(path, { method: 'POST', body: body === undefined ? undefined : JSON.stringify(body) });
export const put = (path, body) => api(path, { method: 'PUT', body: JSON.stringify(body) });
export const del = path => api(path, { method: 'DELETE' });

// 二进制资源(二维码)直接返回 URL,由 <img> 加载
export const qrUrl = (userId, format) => `${BASE}users/${userId}/qr?format=${format}&_=${Date.now()}`;
