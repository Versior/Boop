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

  /* 所有写请求共用的 JSON 封装：带 CSRF、同源 Cookie，401 直接去登录页。 */
  function apiRequest(path, method, body) {
    var headers = {};
    var token = csrfToken();
    if (token) { headers['X-CSRF-Token'] = token; }
    if (body !== undefined) { headers['Content-Type'] = 'application/json'; }

    return fetch(path, {
      method: method,
      credentials: 'same-origin',
      headers: headers,
      body: body === undefined ? undefined : JSON.stringify(body)
    }).then(function (response) {
      return response.json().catch(function () { return null; }).then(function (payload) {
        if (response.status === 401) {
          /* 会话已失效：去登录页比留一个无提示的按钮更清楚。 */
          window.location.assign('/login');
          throw new Error('请先登录');
        }
        if (response.status === 429 && payload && payload.retry_after) {
          throw new Error(errorText(payload, '请求过于频繁') + '（' + payload.retry_after + ' 秒后可重试）');
        }
        if (!response.ok) {
          throw new Error(errorText(payload, '请求失败（' + response.status + '）'));
        }
        return payload ? payload.data : null;
      });
    });
  }

  function reloadSoon() {
    window.setTimeout(function () { window.location.reload(); }, 700);
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
    /* 每次选择新文件都会递增：只有最新一次上传可以写 asset、提示、错误和
       uploading，旧请求返回时就无效了，不会覆盖新图片。 */
    var uploadGeneration = 0;

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
      var generation = ++uploadGeneration;
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
        if (generation !== uploadGeneration) { return; }
        asset = data;
        setHint(data.reused
          ? '这张图片之前已经上传过，将直接复用（' + data.mime_type + '）。'
          : '已上传：' + data.mime_type + ' · ' + Math.round(data.size_bytes / 1024) + ' KB');
      }).catch(function (error) {
        if (generation !== uploadGeneration) { return; }
        asset = null;
        clearPreview();
        if (fileInput) { fileInput.value = ''; }
        setHint(FILE_HINT);
        showError(error.message || '上传失败，请稍后重试。');
      }).then(function () {
        if (generation !== uploadGeneration) { return; }
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
        /* 新一次选择让进行中的旧上传立即失效，并先丢掉旧图片。 */
        uploadGeneration++;
        asset = null;
        uploading = false;
        var chosen = fileInput.files && fileInput.files[0];
        if (!chosen) {
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

  /* 互动：点赞、收藏、评论、回复、删除与站长审核。全部走真实接口，不复制模板。 */
  function wireReactions() {
    var buttons = Array.prototype.slice.call(document.querySelectorAll('[data-reaction]'));
    buttons.forEach(function (button) {
      button.addEventListener('click', function () {
        if (button.disabled) { return; }
        var kind = button.getAttribute('data-reaction');
        var postID = button.getAttribute('data-post-id');
        /* data-on 是服务端渲染的真实状态，点击就是它的反操作。 */
        var next = button.getAttribute('data-on') !== 'true';
        button.disabled = true;

        apiRequest('/api/v1/posts/' + postID + '/' + kind, next ? 'PUT' : 'DELETE').then(function (data) {
          var on = kind === 'like' ? data.liked : data.bookmarked;
          applyReaction(button, on, kind === 'like' ? data.like_count : null);
        }).catch(function (error) {
          if (!window.location.pathname) { return; }
          showToast(error.message || '操作失败，请稍后重试。');
        }).then(function () {
          button.disabled = false;
        });
      });
    });
  }

  function applyReaction(button, on, count) {
    button.setAttribute('data-on', on ? 'true' : 'false');
    button.setAttribute('aria-pressed', on ? 'true' : 'false');
    if (on) { button.classList.add('is-on'); } else { button.classList.remove('is-on'); }
    var counter = button.querySelector('[data-reaction-count]');
    /* 收藏数是当前用户的私有总数，不写进卡片。 */
    if (counter && typeof count === 'number') { counter.textContent = String(count); }
  }

  function wireCommentForm() {
    var form = document.querySelector('[data-comment-form]');
    if (!form) { return; }

    var text = form.querySelector('[data-comment-text]');
    var parentField = form.querySelector('[data-comment-parent]');
    var submit = form.querySelector('[data-comment-submit]');
    var errorBox = form.querySelector('[data-comment-error]');
    var replying = form.querySelector('[data-comment-replying]');
    var replyName = form.querySelector('[data-comment-reply-name]');
    var cancel = form.querySelector('[data-comment-cancel-reply]');

    function show(message) {
      if (!errorBox) { return; }
      errorBox.textContent = message;
      errorBox.hidden = false;
    }

    function startReply(id, name) {
      parentField.value = id;
      if (replyName) { replyName.textContent = name; }
      if (replying) { replying.hidden = false; }
      text.focus();
    }

    function stopReply() {
      parentField.value = '';
      if (replying) { replying.hidden = true; }
    }

    if (cancel) { cancel.addEventListener('click', stopReply); }
    Array.prototype.forEach.call(document.querySelectorAll('[data-comment-reply-to]'), function (button) {
      button.addEventListener('click', function () {
        startReply(button.getAttribute('data-comment-reply-to'), button.getAttribute('data-comment-reply-name'));
      });
    });

    form.addEventListener('submit', function (event) {
      event.preventDefault();
      if (errorBox) { errorBox.hidden = true; }
      var body = text.value.trim();
      if (!body) { show('请先写下内容再提交。'); return; }

      var payload = { body: body };
      if (parentField && parentField.value) { payload.parent_id = Number(parentField.value); }
      submit.disabled = true;

      apiRequest(form.getAttribute('action'), 'POST', payload).then(function (data) {
        stopReply();
        text.value = '';
        showToast(data && data.status === 'pending' ? '评论已提交，等待站长审核' : '评论已发布');
        reloadSoon();
      }).catch(function (error) {
        show(error.message || '评论失败，请稍后重试。');
        submit.disabled = false;
      });
    });
  }

  function wireCommentDeletes() {
    Array.prototype.forEach.call(document.querySelectorAll('[data-comment-delete]'), function (button) {
      button.addEventListener('click', function () {
        if (!window.confirm('确定删除这条评论吗？')) { return; }
        button.disabled = true;
        apiRequest('/api/v1/comments/' + button.getAttribute('data-comment-delete'), 'DELETE').then(function () {
          showToast('评论已删除');
          reloadSoon();
        }).catch(function (error) {
          button.disabled = false;
          showToast(error.message || '删除失败，请稍后重试。');
        });
      });
    });
  }

  function wireModeration() {
    Array.prototype.forEach.call(document.querySelectorAll('[data-moderation]'), function (button) {
      button.addEventListener('click', function () {
        var action = button.getAttribute('data-moderation');
        var id = button.getAttribute('data-comment-id');
        if (action === 'delete' && !window.confirm('确定删除这条评论吗？')) { return; }

        var endpoint = '/api/v1/admin/comments/' + id;
        var method = 'DELETE';
        if (action === 'approve' || action === 'reject') {
          endpoint += '/' + action;
          method = 'POST';
        }
        button.disabled = true;
        apiRequest(endpoint, method).then(function () {
          showToast('已' + ({ approve: '批准', reject: '拒绝', delete: '删除' }[action] || '处理'));
          reloadSoon();
        }).catch(function (error) {
          button.disabled = false;
          showToast(error.message || '操作失败，请稍后重试。');
        });
      });
    });
  }

  /* 站长设置：把表单以 PATCH JSON 提交；空密钥表示保持已保存的值。 */
  function wireSettingsForm() {
    var form = document.querySelector('[data-settings-form]');
    if (!form) { return; }

    var submit = form.querySelector('[data-settings-submit]');
    var errorBox = form.querySelector('[data-settings-error]');
    /* 这两个字段在服务端是整数，Number() 避免把 "20" 当成字符串提交。 */
    var NUMBER_FIELDS = ['page_size', 'ai_author_status_ttl_hours'];

    function show(message) {
      if (!errorBox) { return; }
      errorBox.textContent = message;
      errorBox.hidden = false;
    }

    form.addEventListener('submit', function (event) {
      event.preventDefault();
      if (errorBox) { errorBox.hidden = true; }
      if (submit) { submit.disabled = true; }

      var payload = {};
      var clear = [];
      Array.prototype.forEach.call(form.elements, function (field) {
        var name = field.getAttribute('name');
        if (field.type === 'checkbox') {
          if (name) { payload[name] = field.checked; }
          return;
        }
        if (!name) { return; }
        if (field.type === 'password') {
          /* 留空的密钥字段不发送，服务端就不会覆盖已保存的密钥。 */
          if (field.value) { payload[name] = field.value; }
          return;
        }
        if (NUMBER_FIELDS.indexOf(name) >= 0) {
          payload[name] = Number(field.value);
          return;
        }
        payload[name] = field.value;
      });
      Array.prototype.forEach.call(form.querySelectorAll('[data-secret-clear]'), function (box) {
        if (box.checked) { clear.push(box.getAttribute('data-secret-clear')); }
      });
      if (clear.length) { payload.clear_secret = clear; }

      apiRequest(form.getAttribute('data-settings-action'), 'PATCH', payload).then(function () {
        showToast('设置已保存');
        /* 重新加载页面，让密钥的“已配置”状态和服务端渲染一起刷新。 */
        window.setTimeout(function () { window.location.reload(); }, 900);
      }).catch(function (error) {
        if (submit) { submit.disabled = false; }
        show(error.message || '保存失败，请稍后重试。');
      });
    });
  }

  function wireSocial() {
    wireReactions();
    wireCommentForm();
    wireCommentDeletes();
    wireModeration();
  }

  document.addEventListener('DOMContentLoaded', function () {
    Array.prototype.forEach.call(document.querySelectorAll('[data-theme-toggle]'), function (btn) {
      btn.addEventListener('click', function () {
        apply(root.getAttribute('data-theme') === 'dark' ? 'light' : 'dark', true);
      });
    });

    Array.prototype.forEach.call(document.querySelectorAll('[data-auth-form]'), wireAuthForm);
    wireComposer();
    wireSettingsForm();
    wireSocial();
  });
})();
