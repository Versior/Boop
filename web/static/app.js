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

  /* 轻提示：V7 的 toast 行为（3.4 秒后自动隐藏）。 */
  var toastTimer = null;
  function showToast(message) {
    var toast = document.getElementById('toast');
    if (!toast) { return; }
    var box = toast.querySelector('[data-toast-message]');
    if (box) { box.textContent = message; }
    toast.hidden = false;
    void toast.offsetWidth;
    toast.classList.add('show');
    window.clearTimeout(toastTimer);
    toastTimer = window.setTimeout(function () {
      toast.classList.remove('show');
      window.setTimeout(function () { toast.hidden = true; }, 200);
    }, 3400);
  }

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

  /* 快捷发布器：仅在站长 DOM 中存在，直接调用 /api/v1/admin/posts。 */
  function wireComposer() {
    var composer = document.querySelector('[data-composer]');
    if (!composer) { return; }

    var form = composer.querySelector('[data-composer-form]');
    var extra = composer.querySelector('[data-composer-extra]');
    var text = composer.querySelector('[data-composer-text]');
    var title = composer.querySelector('[data-composer-title]');
    var tags = composer.querySelector('[data-composer-tags]');
    var excerpt = composer.querySelector('[data-composer-excerpt]');
    var drop = composer.querySelector('[data-composer-drop]');
    var count = composer.querySelector('[data-composer-count]');
    var submit = composer.querySelector('[data-composer-submit]');
    var errorBox = composer.querySelector('[data-composer-error]');
    var modes = Array.prototype.slice.call(composer.querySelectorAll('[data-mode]'));
    var fileInput = composer.querySelector('[data-composer-file]');
    var fileHint = composer.querySelector('[data-composer-file-hint]');
    var preview = composer.querySelector('[data-composer-preview]');

    var mode = currentMode(modes);
    /* 动态上限来自 docs/PRODUCT.md（2,000 字），文章与摄影没有单独字数上限。 */
    var MOMENT_MAX = 2000;
    var FILE_HINT = fileHint ? fileHint.textContent : '';
    /* 摄影模式先上传图片拿到 asset id，再用它创建 photo 内容。 */
    var asset = null;
    var uploading = false;
    var previewURL = '';

    function currentMode(buttons) {
      for (var i = 0; i < buttons.length; i++) {
        if (buttons[i].getAttribute('aria-pressed') === 'true') { return buttons[i].getAttribute('data-mode'); }
      }
      return 'moment';
    }

    function hasContent() {
      if (mode === 'photo') { return !!asset; }
      if (text.value.trim()) { return true; }
      /* 文章只有标题也算写了内容，与 V7 的原型行为一致。 */
      return mode === 'article' && !!title.value.trim();
    }

    function sync() {
      if (mode === 'moment') {
        count.textContent = text.value.length + '/' + MOMENT_MAX;
      } else {
        count.textContent = String(text.value.length);
      }
      submit.disabled = uploading || !hasContent();
    }

    function grow() {
      text.style.height = 'auto';
      text.style.height = Math.min(text.scrollHeight, 240) + 'px';
    }

    function showError(message) {
      if (!errorBox) { return; }
      errorBox.textContent = message;
      errorBox.hidden = false;
    }

    function clearError() {
      if (!errorBox) { return; }
      errorBox.textContent = '';
      errorBox.hidden = true;
    }

    function setHint(message) {
      if (fileHint) { fileHint.textContent = message; }
    }

    function clearPreview() {
      if (previewURL) {
        URL.revokeObjectURL(previewURL);
        previewURL = '';
      }
      if (preview) {
        preview.hidden = true;
        preview.removeAttribute('src');
      }
    }

    function showPreview(file) {
      if (!preview || !window.URL || !URL.createObjectURL) { return; }
      clearPreview();
      previewURL = URL.createObjectURL(file);
      preview.src = previewURL;
      preview.hidden = false;
    }

    /* 上传失败时把服务端的中文错误直接展示给用户，不猜测原因。 */
    function uploadImage(file) {
      var body = new FormData();
      body.append('file', file, file.name);

      var headers = {};
      var token = csrfToken();
      if (token) { headers['X-CSRF-Token'] = token; }

      uploading = true;
      sync();
      setHint('上传中…');

      return fetch('/api/v1/admin/uploads', {
        method: 'POST',
        credentials: 'same-origin',
        headers: headers,
        body: body
      }).then(function (response) {
        return response.json().catch(function () { return null; }).then(function (result) {
          if (!response.ok) {
            throw new Error(errorText(result, '上传失败（' + response.status + '）'));
          }
          return result.data;
        });
      }).then(function (data) {
        asset = data;
        setHint(data.reused
          ? '这张图片之前已经上传过，将直接复用（' + data.mime_type + '）。'
          : '已上传：' + data.mime_type + ' · ' + Math.round(data.size_bytes / 1024) + ' KB');
      }).catch(function (error) {
        asset = null;
        clearPreview();
        if (fileInput) { fileInput.value = ''; }
        setHint(FILE_HINT);
        showError(error.message || '上传失败，请稍后重试。');
      }).then(function () {
        uploading = false;
        sync();
      });
    }

    function applyMode(next) {
      mode = next;
      modes.forEach(function (btn) {
        btn.setAttribute('aria-pressed', btn.getAttribute('data-mode') === next ? 'true' : 'false');
      });
      if (title) { title.hidden = next !== 'article'; }
      if (tags) { tags.hidden = next === 'moment'; }
      if (excerpt) { excerpt.hidden = next !== 'article'; }
      if (drop) { drop.hidden = next !== 'photo'; }
      sync();
    }

    function open() {
      if (extra) { extra.hidden = false; }
      sync();
    }

    function payload() {
      var body = {
        type: mode,
        status: 'published',
        body: text.value.trim()
      };
      if (mode === 'article') {
        body.title = title ? title.value.trim() : '';
        if (excerpt && excerpt.value.trim()) { body.excerpt = excerpt.value.trim(); }
      }
      if (tags && tags.value.trim()) {
        body.tags = tags.value.split(',').map(function (item) { return item.trim(); }).filter(function (item) { return item; });
      }
      if (mode === 'photo' && asset) { body.asset_ids = [asset.id]; }
      return body;
    }

    modes.forEach(function (btn) {
      btn.addEventListener('click', function () {
        open();
        applyMode(btn.getAttribute('data-mode'));
      });
    });
    if (text) {
      text.addEventListener('focus', open);
      text.addEventListener('input', function () { open(); grow(); sync(); });
    }
    if (title) { title.addEventListener('input', function () { open(); sync(); }); }
    if (fileInput) {
      fileInput.addEventListener('change', function () {
        clearError();
        open();
        var chosen = fileInput.files && fileInput.files[0];
        if (!chosen) {
          asset = null;
          clearPreview();
          setHint(FILE_HINT);
          sync();
          return;
        }
        showPreview(chosen);
        uploadImage(chosen);
      });
    }

    form.addEventListener('submit', function (event) {
      event.preventDefault();
      clearError();
      if (mode === 'photo' && !asset) {
        showError('请先选择一张图片再发布摄影内容。');
        sync();
        return;
      }
      submit.disabled = true;

      var headers = { 'Content-Type': 'application/json' };
      var token = csrfToken();
      if (token) { headers['X-CSRF-Token'] = token; }

      fetch(form.getAttribute('action'), {
        method: 'POST',
        credentials: 'same-origin',
        headers: headers,
        body: JSON.stringify(payload())
      }).then(function (response) {
        return response.json().catch(function () { return null; }).then(function (result) {
          if (!response.ok) {
            throw new Error(errorText(result, '发布失败（' + response.status + '）'));
          }
          return result;
        });
      }).then(function () {
        text.value = '';
        if (title) { title.value = ''; }
        if (tags) { tags.value = ''; }
        if (excerpt) { excerpt.value = ''; }
        if (fileInput) { fileInput.value = ''; }
        asset = null;
        clearPreview();
        setHint(FILE_HINT);
        showToast('已发布');
        /* 重新加载首页，让服务端渲染的新卡片出现（不在 JS 里重复模板）。 */
        window.setTimeout(function () { window.location.reload(); }, 900);
      }).catch(function (error) {
        showError(error.message || '发布失败，请稍后重试。');
        sync();
      });
    });

    applyMode(mode);
  }

  document.addEventListener('DOMContentLoaded', function () {
    Array.prototype.forEach.call(document.querySelectorAll('[data-theme-toggle]'), function (btn) {
      btn.addEventListener('click', function () {
        apply(root.getAttribute('data-theme') === 'dark' ? 'light' : 'dark', true);
      });
    });

    Array.prototype.forEach.call(document.querySelectorAll('[data-auth-form]'), wireAuthForm);
    wireComposer();
  });
})();
