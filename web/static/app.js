/* Boop shell behaviour: theme bootstrap, theme toggle. No network, no framework. */
(function () {
  'use strict';

  var root = document.documentElement;
  var STORAGE_KEY = 'boop-theme';
  var LABELS = { light: '切换到深色主题', dark: '切换到浅色主题' };

  function stored() {
    try {
      var saved = localStorage.getItem(STORAGE_KEY);
      if (saved === 'dark' || saved === 'light') { return saved; }
    } catch (e) { /* localStorage 不可用时忽略 */ }
    if (window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches) { return 'dark'; }
    return 'light';
  }

  function apply(theme, persist) {
    var value = theme === 'dark' ? 'dark' : 'light';
    var label = LABELS[value];
    root.setAttribute('data-theme', value);
    Array.prototype.forEach.call(document.querySelectorAll('[data-theme-toggle]'), function (btn) {
      /* 有可见文字入口（外观）把文字并入可访问名称，图标入口直接用动作描述 */
      var prefix = btn.getAttribute('data-theme-prefix');
      btn.setAttribute('aria-label', prefix ? (prefix + '：' + label) : label);
      btn.setAttribute('title', label);
    });
    if (persist) {
      try { localStorage.setItem(STORAGE_KEY, value); } catch (e) { /* 忽略写入失败 */ }
    }
  }

  /* 主题引导：本脚本在 <head> 中同步执行，避免深色主题闪白。 */
  apply(stored(), false);

  /* 认证表单：以 JSON 提交，失败时把错误消息写在表单内。 */
  function csrfToken() {
    var meta = document.querySelector('meta[name="csrf-token"]');
    return meta ? meta.getAttribute('content') : '';
  }

  function errorText(payload, fallback) {
    if (payload && payload.error && payload.error.message) { return payload.error.message; }
    return fallback;
  }

  function wireAuthForm(form) {
    var box = form.querySelector('[data-auth-error]');
    var submit = form.querySelector('button[type="submit"]');

    function show(message) {
      if (!box) { return; }
      box.textContent = message;
      box.hidden = false;
    }

    form.addEventListener('submit', function (event) {
      event.preventDefault();
      if (box) { box.hidden = true; }
      if (submit) { submit.disabled = true; }

      var payload = {};
      Array.prototype.forEach.call(form.elements, function (field) {
        if (field.name) { payload[field.name] = field.value; }
      });

      var headers = { 'Content-Type': 'application/json' };
      var token = csrfToken();
      if (token) { headers['X-CSRF-Token'] = token; }

      fetch(form.getAttribute('action'), {
        method: 'POST',
        credentials: 'same-origin',
        headers: headers,
        body: JSON.stringify(payload)
      }).then(function (response) {
        return response.json().catch(function () { return null; }).then(function (body) {
          if (!response.ok) {
            throw new Error(errorText(body, '请求失败（' + response.status + '）'));
          }
          return body;
        });
      }).then(function () {
        window.location.assign(form.getAttribute('data-auth-success') || '/');
      }).catch(function (error) {
        show(error.message || '请求失败，请稍后重试。');
        if (submit) { submit.disabled = false; }
      });
    });
  }

  document.addEventListener('DOMContentLoaded', function () {
    Array.prototype.forEach.call(document.querySelectorAll('[data-theme-toggle]'), function (btn) {
      btn.addEventListener('click', function () {
        apply(root.getAttribute('data-theme') === 'dark' ? 'light' : 'dark', true);
      });
    });

    Array.prototype.forEach.call(document.querySelectorAll('[data-auth-form]'), wireAuthForm);
  });
})();
