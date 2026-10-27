// 登录页：提交 JSON，成功后回到登录前的页面（只接受本站路径）。
import { $ } from './app.js';

function nextPath() {
  const next = new URLSearchParams(location.search).get('next') || '/';
  // 只接受以 / 开头的本站路径，防止被拿来跳转到外站（//evil.com、/\evil.com）。
  return next.startsWith('/') && !next.startsWith('//') && !next.startsWith('/\\') ? next : '/';
}

$('login-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const error = $('login-error');
  const button = $('login-submit');
  error.textContent = '';
  button.disabled = true;
  try {
    const res = await fetch('/api/v1/login', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
      body: JSON.stringify({ username: $('username').value, password: $('password').value }),
    });
    if (res.ok) {
      location.href = nextPath();
      return;
    }
    const body = await res.json().catch(() => null);
    error.textContent = (body && body.message) || `登录失败（${res.status}）`;
  } catch {
    error.textContent = '网络错误，请稍后再试';
  } finally {
    button.disabled = false;
  }
});
