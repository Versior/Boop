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

  document.addEventListener('DOMContentLoaded', function () {
    Array.prototype.forEach.call(document.querySelectorAll('[data-theme-toggle]'), function (btn) {
      btn.addEventListener('click', function () {
        apply(root.getAttribute('data-theme') === 'dark' ? 'light' : 'dark', true);
      });
    });
  });
})();
