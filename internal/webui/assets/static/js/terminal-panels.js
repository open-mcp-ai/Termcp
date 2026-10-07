/** Spin a button's icon once. A refresh whose result is a list that already
 *  looked right has no visible effect, so the turn is the only acknowledgement
 *  the press gets. The transition is cleared on the timer, not left in place:
 *  a second press must start a fresh turn instead of easing back from 360deg. */
function spinIconOnce(btn) {
  var svg = btn && btn.querySelector('svg');
  if (!svg) return;
  svg.style.transition = 'transform 0.6s ease';
  svg.style.transform = 'rotate(360deg)';
  setTimeout(function() { svg.style.transition = 'none'; svg.style.transform = 'rotate(0deg)'; }, 1220);
}

function _initShellWindowUI(win, connLabel, sessionId) {
  var header = win.querySelector('.shell-window-header');
  var collapseBtn = win.querySelector('.shell-window-collapse-btn');
  var closeBtn = win.querySelector('.close-btn');
  var minBtn = win.querySelector('.shell-window-min-btn');
  bindShellWindowMouseToFront(win);
  if (collapseBtn) collapseBtn.style.display = '';
  if (closeBtn) {
    closeBtn.setAttribute('data-i18n-title', 'session.delete.title');
    closeBtn.title = t('session.delete.title');
  }
  bindShellWindowMinButton(win, minBtn);
  var titleCopyBtn = win.querySelector('.title-copy-btn');
  if (titleCopyBtn) {
    titleCopyBtn.style.display = '';
    titleCopyBtn.addEventListener('mousedown', function (e) { e.stopPropagation(); });
    titleCopyBtn.addEventListener('click', function (e) {
      e.preventDefault();
      e.stopPropagation();
      copyTextToClipboard(resourceUrlSession(sessionId)).then(function () { showCopyToast(); }).catch(function () { showCopyToast(t('toast.copy.failed')); });
    });
  }

  bindShellWindowCloseButton(win, closeBtn);

  win._inputClosed = false;
  setupTermScrollFab(win);

  collapseBtn.addEventListener('mousedown', function (e) { e.stopPropagation(); });
  collapseBtn.addEventListener('click', function (e) {
    e.preventDefault(); e.stopPropagation();
    if (win.classList.contains('shell-window-collapsed')) expandShellWindow(win);
    else collapseShellWindow(win);
  });

  setupShellWindowDrag(win, header);
  setupShellWindowResize(win);
  setupMobileSessionSwitcher(win);

  // --- Header tab switching (term / fw / file / notify panels) ---
  // Inject the Notifications tab here so both the pending and connected
  // window templates get it without duplicating the tab-bar markup.
  var tabBarEl = win.querySelector('.shell-tab-bar');
  if (tabBarEl && !tabBarEl.querySelector('.shell-tab-btn[data-stab="notify"]')) {
    var ntfTabBtn = document.createElement('button');
    ntfTabBtn.type = 'button';
    ntfTabBtn.className = 'shell-tab-btn';
    ntfTabBtn.setAttribute('data-stab', 'notify');
    ntfTabBtn.setAttribute('data-i18n-title', 'win.notify.tip');
    ntfTabBtn.title = t('win.notify.tip');
    ntfTabBtn.innerHTML = '<svg viewBox="0 0 14 14" width="14" height="14" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round"><path d="M3.5 6a3.5 3.5 0 017 0c0 2.5 1 3.5 1 3.5H2.5s1-1 1-3.5z"/><path d="M5.8 11.5a1.2 1.2 0 002.4 0"/></svg>';
    tabBarEl.appendChild(ntfTabBtn);
  }
  /* The lock is declared here — before the pane tabs, so the markup still reads
     gate-then-faces and no template has to know about the bell. The bell is
     injected below and the lock is then moved after it, so the row reads
     term · fw · file · notify · review: the two state controls sit together at
     the end, away from the navigation. */
  var reviewBtn = tabBarEl && tabBarEl.querySelector('.shell-review-lock');
  var notifyBtn = tabBarEl && tabBarEl.querySelector('.shell-tab-btn[data-stab="notify"]');
  if (reviewBtn && notifyBtn) tabBarEl.appendChild(reviewBtn);
  var tabBtns = win.querySelectorAll('.shell-tab-btn');
  var terminalWrap = win.querySelector('.shell-terminal-wrap');
  var tabPanels = win.querySelectorAll('.shell-tab-panel');
  tabBtns.forEach(function(btn) {
    btn.addEventListener('click', function(e) {
      e.preventDefault(); e.stopPropagation();
      var tab = this.getAttribute('data-stab');
      tabBtns.forEach(function(b) { b.classList.toggle('active', b === btn); });
      if (terminalWrap) terminalWrap.style.display = tab === 'term' ? '' : 'none';
      tabPanels.forEach(function(p) { p.style.display = p.getAttribute('data-stab') === tab ? 'flex' : 'none'; });
      if (tab === 'term') {
        var ch = win._activeChannelSid && win._channels && win._channels[win._activeChannelSid];
        if (ch && ch.term && ch.instEl) {
          requestAnimationFrame(function() {
            requestAnimationFrame(function() {
              fitShellTerminal(ch.term, ch.instEl, win, true);
              try { ch.term.focus(); } catch(e2) {}
            });
          });
        }
      }
      if (tab === 'fw') refreshShellFwList(win);
      if (tab === 'notify') refreshShellNtfList(win);
    });
  });

  // Forward "add" button
  var fwAddBtn = win.querySelector('.shell-fw-add-btn');
  if (fwAddBtn) {
    fwAddBtn.addEventListener('click', function(e) {
      e.stopPropagation();
      /* No profile is passed: the server labels the forward from the session it
         is created on (handleCreateForward reads the session's own name), and
         this window's connLabel cannot know the server's answer anyway. */
      openForwardModal(sessionId);
    });
  }
  var fwRefreshBtn = win.querySelector('.shell-fw-refresh-btn');
  if (fwRefreshBtn) {
    fwRefreshBtn.addEventListener('click', function(e) {
      e.stopPropagation();
      spinIconOnce(this);
      loadForwards();
    });
  }

  // Refresh forwardings list for this window (from global cache)
  function refreshShellFwList(w) {
    w = w || win;
    var listEl = w.querySelector('.shell-fw-list');
    if (!listEl) return;
    var cfg = connLabel || 'internal';
    var fwds = (window._lastForwards || []).filter(function(f) { return forwardMatchesConfig(f, cfg); });
    if (!fwds.length) { listEl.innerHTML = '<div style="padding:16px;color:#8b949e;text-align:center">' + escapeHtml(t('fw.empty')) + '</div>'; return; }
    listEl.innerHTML = fwds.map(function(f){
      var dirLabel = f.direction;
      var dirColor = dirLabel === 'local' ? '#3fb950' : (dirLabel === 'dynamic' ? '#58a6ff' : '#d29922');
      return '<div style="display:flex;align-items:center;gap:10px;padding:10px 12px;border-bottom:1px solid #21262d;transition:background .1s" onmouseover="this.style.background=\'#161b22\'" onmouseout="this.style.background=\'\'">' +
        '<span style="font-size:0.65rem;font-weight:600;text-transform:uppercase;padding:1px 5px;border-radius:3px;color:' + dirColor + ';border:1px solid ' + dirColor + ';flex-shrink:0;min-width:42px;text-align:center">' + escapeHtml(dirLabel) + '</span>' +
        '<span style="flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:0.78rem"><span style="color:#8b949e">' + escapeHtml(f.listen_addr) + '</span> <span style="color:#484f58">→</span> <span style="color:#c9d1d9">' + escapeHtml(f.target_addr) + '</span></span>' +
        '<button class="shell-fw-del-btn" data-fwid="' + escapeHtml(f.forward_id) + '" style="padding:2px 6px;font-size:0.68rem;border:1px solid transparent;border-radius:3px;background:transparent;color:#484f58;cursor:pointer;flex-shrink:0" onmouseover="this.style.borderColor=\'#f85149\';this.style.color=\'#f85149\'" onmouseout="this.style.borderColor=\'transparent\';this.style.color=\'#484f58\'">✕</button>' +
        '</div>';
    }).join('');
    listEl.querySelectorAll('.shell-fw-del-btn').forEach(function(btn) {
      btn.addEventListener('click', function(e) {
        e.stopPropagation();
        deleteForward(this.getAttribute('data-fwid')).catch(function(){});
      });
    });
    listEl.scrollTop = listEl.scrollHeight;
  }
  win._refreshFwList = function() { refreshShellFwList(win); };

  // Refresh notifications list for this window (from global cache).
  function refreshShellNtfList(w) {
    w = w || win;
    var listEl = w.querySelector('.shell-ntf-list');
    if (!listEl) return;
    var rules = (window._lastNotifications || []).filter(function(n) { return n.session_id === sessionId; });
    if (!rules.length) {
      // This tab lists wake-up RULES (the ones the notify tool registers), not the
      // toasts that appear in the corner. Those are two different things, and an
      // empty panel beside a toast that just popped reads as a bug — so the empty
      // state says which thing is empty.
      listEl.innerHTML = '<div style="padding:16px;color:#8b949e;text-align:center;line-height:1.6">'
        + escapeHtml(t('ntf.empty'))
        + '<div style="margin-top:6px;font-size:0.72rem;color:#6e7681">'
        + escapeHtml(t('ntf.emptyHint'))
        + '</div></div>';
      return;
    }
    listEl.innerHTML = rules.map(function(n){
      var evColor = n.event === 'exit' ? '#f85149' : (n.event === 'silence' ? '#d29922' : '#3fb950');
      var extra = (n.event === 'silence' && n.silence_seconds) ? ' ' + n.silence_seconds + 's' : '';
      var chLabel = n.channel === 'sampling' ? t('ntf.channel.sampling') : t('ntf.channel.resource');
      var unregisterTip = t('ntf.unregister');
      return '<div style="display:flex;align-items:center;gap:10px;padding:10px 12px;border-bottom:1px solid #21262d" onmouseover="this.style.background=\'#161b22\'" onmouseout="this.style.background=\'\'">' +
        '<span style="font-size:0.65rem;font-weight:600;text-transform:uppercase;padding:1px 5px;border-radius:3px;color:' + evColor + ';border:1px solid ' + evColor + ';flex-shrink:0;min-width:56px;text-align:center">' + escapeHtml(n.event) + escapeHtml(extra) + '</span>' +
        '<span style="flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:0.78rem"><span style="color:#8b949e">' + escapeHtml(chLabel) + '</span> <span style="color:#484f58">·</span> <span style="color:#c9d1d9">' + escapeHtml(n.shell_id) + '</span></span>' +
        '<button class="shell-ntf-del-btn" data-ntfid="' + escapeHtml(n.rule_id) + '" title="' + unregisterTip + '" style="padding:2px 6px;font-size:0.68rem;border:1px solid transparent;border-radius:3px;background:transparent;color:#484f58;cursor:pointer;flex-shrink:0" onmouseover="this.style.borderColor=\'#f85149\';this.style.color=\'#f85149\'" onmouseout="this.style.borderColor=\'transparent\';this.style.color=\'#484f58\'">✕</button>' +
        '</div>';
    }).join('');
    listEl.querySelectorAll('.shell-ntf-del-btn').forEach(function(btn) {
      btn.addEventListener('click', function(e) {
        e.stopPropagation();
        deleteNotification(this.getAttribute('data-ntfid')).then(function(){ loadNotifications(); }).catch(function(){});
      });
    });
  }
  win._refreshNtfList = function() { refreshShellNtfList(win); };
  var ntfRefreshBtn = win.querySelector('.shell-ntf-refresh-btn');
  if (ntfRefreshBtn) {
    ntfRefreshBtn.addEventListener('click', function(e) {
      e.stopPropagation();
      spinIconOnce(this);
      loadNotifications();
    });
  }

  // File operations for this shell window
  var filePathInput = win.querySelector('.shell-file-path');
  var fileListing = win.querySelector('.shell-file-listing');
  var fileUploadInput = win.querySelector('.shell-file-upload-input');
  var fileCtxMenu = win.querySelector('.shell-file-ctx-menu');

  // Dismiss context menu on outside click. Scoped to the window: without
  // removeEventListener every opened window left a click handler (and its
  // closed-over DOM) attached to document forever.
  var onFileCtxOutsideClick = function(e) {
    if (!fileCtxMenu || fileCtxMenu.style.display === 'none') return;
    if (!fileCtxMenu.contains(e.target)) fileCtxMenu.style.display = 'none';
  };
  document.addEventListener('click', onFileCtxOutsideClick);
  addWinDisposable(win, function() {
    document.removeEventListener('click', onFileCtxOutsideClick);
  });

  function shellFileBrowse() {
    var path = filePathInput.value.trim() || '/';
    if (!fileListing) return;
    fileListing.innerHTML = '<div style="padding:8px;color:#8b949e">' + escapeHtml(t('common.loading')) + '</div>';
    if (fileCtxMenu) fileCtxMenu.style.display = 'none';
    fetch('/api/sessions/' + encodeURIComponent(sessionId) + '/files?path=' + encodeURIComponent(path))
      .then(function(r) { if (!r.ok) throw new Error(r.status); return r.json(); })
      .then(function(data) { renderShellFileList(data, path); })
      .catch(function(e) { fileListing.innerHTML = '<div style="padding:8px;color:#f85149">' + escapeHtml(t('file.loadFailed', { msg: String(e.message||e) })) + '</div>'; });
  }

  // The path the context menu was opened on. One variable for the whole window:
  // the menu is a single node, so only one row can have opened it.
  var _ctxPath = '';

  /* One implementation of the three file actions, shared by the context menu and
     the detail view's buttons. `name` is only the rename prompt's default; both
     callers can supply it, and the listing's basename is the fallback — the two
     used to carry separate copies that had already drifted apart in the prompt
     they showed for the same action. */
  function fileMenuAction(action, path, name) {
    if (fileCtxMenu) fileCtxMenu.style.display = 'none';
    if (!path) return;
    var base = path.replace(/\/+$/, '');
    if (!name) name = base.split('/').pop() || '';

    if (action === 'download') {
      window.open('/api/sessions/' + encodeURIComponent(sessionId) + '/files/download?path=' + encodeURIComponent(path), '_blank');
    } else if (action === 'delete') {
      if (!confirm(t('file.deleteConfirm', { path: path }))) return;
      fetch('/api/sessions/' + encodeURIComponent(sessionId) + '/files?path=' + encodeURIComponent(path), {method:'DELETE'})
        .then(function(r) { if (!r.ok) throw new Error(r.status); return r.json(); })
        .then(function() { shellFileBrowse(); })
        .catch(function(e) { alert(t('toast.delete.failed', { msg: e.message })); });
    } else if (action === 'rename') {
      var newName = prompt(t('file.renamePrompt', { name: name }), name);
      if (!newName || newName === name) return;
      var parts = base.split('/'); parts.pop();
      var to = (parts.join('/') || '') + '/' + newName;
      fetch('/api/sessions/' + encodeURIComponent(sessionId) + '/files?from=' + encodeURIComponent(path) + '&to=' + encodeURIComponent(to), {method:'PUT'})
        .then(function(r) { if (!r.ok) throw new Error(r.status); return r.json(); })
        .then(function() { shellFileBrowse(); })
        .catch(function(e) { alert(t('toast.rename.failed', { msg: e.message })); });
    }
  }

  function showFileMenu(e, path, isDir) {
    e.preventDefault();
    e.stopPropagation();
    _ctxPath = path;
    if (!fileCtxMenu) return;
    fileCtxMenu.querySelectorAll('.file-ctx-item').forEach(function(it) {
      it.style.display = (it.dataset.action === 'download' && isDir) ? 'none' : '';
    });
    fileCtxMenu.style.display = 'block';
    // Position near cursor, within viewport
    var x = (e.touches && e.touches[0] ? e.touches[0].clientX : e.clientX) || e.pageX || 100;
    var y = (e.touches && e.touches[0] ? e.touches[0].clientY : e.clientY) || e.pageY || 100;
    var mw = fileCtxMenu.offsetWidth || 150;
    var mh = fileCtxMenu.offsetHeight || 120;
    if (x + mw > window.innerWidth) x = window.innerWidth - mw - 4;
    if (y + mh > window.innerHeight) y = window.innerHeight - mh - 4;
    fileCtxMenu.style.left = Math.max(0, x) + 'px';
    fileCtxMenu.style.top = Math.max(0, y) + 'px';
  }

  if (fileCtxMenu) {
    fileCtxMenu.addEventListener('click', function(e) {
      var it = e.target.closest('.file-ctx-item');
      if (!it) return;
      fileMenuAction(it.dataset.action, _ctxPath);
    });
  }

  // Event delegation for file detail action buttons. One path only: the buttons
  // hand their own path and name to the same single action implementation above.
  fileListing.addEventListener('click', function(e) {
    var btn = e.target.closest('[data-file-action]');
    if (!btn) return;
    e.stopPropagation();
    var path = btn.getAttribute('data-file-path') || btn.parentElement.getAttribute('data-file-path');
    if (!path) return;
    fileMenuAction(btn.getAttribute('data-file-action'), path, btn.getAttribute('data-file-name'));
  });

  var _longPressTimer = null;
  function renderShellFileList(data, currentPath) {
    filePathInput.value = currentPath;
    if (!data.is_dir) {
      // File detail view — show metadata with action buttons
      var nm = data.name || currentPath;
      var detailHtml = '<div class="shell-file-detail" data-file-path="' + escapeHtml(currentPath) + '">';
      detailHtml += '<div style="font-size:0.85rem;font-weight:600;margin-bottom:4px;display:flex;align-items:center;gap:6px"><span>📄</span><span style="word-break:break-all">' + escapeHtml(nm) + '</span></div>';
      detailHtml += '<div style="font-size:0.75rem;color:#8b949e;margin-bottom:2px">' + escapeHtml(t('file.size', { size: formatSize(data.size||0) })) + '</div>';
      if (data.mod_time) detailHtml += '<div style="font-size:0.75rem;color:#8b949e;margin-bottom:2px">' + escapeHtml(t('file.modified', { time: fmtTime(data.mod_time) })) + '</div>';
      detailHtml += '<div class="shell-file-detail-actions">';
      detailHtml += '<button class="sf-dl" data-file-action="download" data-file-path="' + escapeHtml(currentPath) + '">' + escapeHtml(t('common.download')) + '</button>';
      detailHtml += '<button data-file-action="rename" data-file-path="' + escapeHtml(currentPath) + '" data-file-name="' + escapeHtml(nm) + '">' + escapeHtml(t('common.rename')) + '</button>';
      detailHtml += '<button class="sf-del" data-file-action="delete" data-file-path="' + escapeHtml(currentPath) + '">' + escapeHtml(t('common.delete')) + '</button>';
      detailHtml += '</div></div>';
      fileListing.innerHTML = detailHtml;
      return;
    }
    var children = data.children || [];
    if (!children.length) { fileListing.innerHTML = '<div style="padding:8px;color:#8b949e">' + escapeHtml(t('file.empty')) + '</div>'; return; }
    var basePath = currentPath.replace(/\/+$/, '');
    fileListing.innerHTML = children.map(function(c) {
      var icon = c.is_dir ? '📁' : '📄';
      var fullPath = basePath + '/' + c.name;
      return '<div class="shell-file-row" data-path="' + escapeHtml(fullPath) + '" data-isdir="' + (c.is_dir?'1':'0') + '" data-name="' + escapeHtml(c.name) + '" style="display:flex;align-items:center;gap:6px;padding:4px 8px;cursor:pointer;border-bottom:1px solid #30363d;white-space:nowrap">' +
        '<span style="flex-shrink:0">' + icon + '</span><span style="flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis">' + escapeHtml(c.name) + '</span>' +
        (!c.is_dir ? '<span style="flex-shrink:0;color:#8b949e;font-size:0.7rem;margin-right:2px">' + formatSize(c.size||0) + '</span>' : '') +
        '<button class="shell-file-menu-btn" title="' + escapeHtml(t('file.menu')) + '" style="flex-shrink:0">&vellip;</button>' +
        '</div>';
    }).join('');
    // Bind row events
    fileListing.querySelectorAll('.shell-file-row').forEach(function(row) {
      var path = row.getAttribute('data-path');
      var isDir = row.getAttribute('data-isdir') === '1';
      row.addEventListener('click', function(e) {
        if (e.target.closest('.shell-file-menu-btn')) return;
        filePathInput.value = path;
        shellFileBrowse();
      });
      var btn = row.querySelector('.shell-file-menu-btn');
      if (btn) btn.addEventListener('click', function(e) { showFileMenu(e, path, isDir); });
      row.addEventListener('contextmenu', function(e) { showFileMenu(e, path, isDir); });
      row.addEventListener('touchstart', function(e) {
        if (e.target.closest('.shell-file-menu-btn')) return;
        clearTimeout(_longPressTimer);
        _longPressTimer = setTimeout(function() { showFileMenu(e, path, isDir); }, 600);
      }, {passive:true});
      row.addEventListener('touchend', function() { clearTimeout(_longPressTimer); });
      row.addEventListener('touchmove', function() { clearTimeout(_longPressTimer); });
    });
  }

  if (win.querySelector('.shell-file-go')) win.querySelector('.shell-file-go').addEventListener('click', function(e) {
    spinIconOnce(this);
    shellFileBrowse();
  });
  if (win.querySelector('.shell-file-up')) win.querySelector('.shell-file-up').addEventListener('click', function() {
    var p = filePathInput.value.trim().replace(/\/+$/, '') || '/';
    if (p === '/') return;
    var parts = p.split('/'); parts.pop();
    filePathInput.value = parts.join('/') || '/';
    shellFileBrowse();
  });
  if (filePathInput) filePathInput.addEventListener('keydown', function(e) { if (e.key === 'Enter') shellFileBrowse(); });
  if (win.querySelector('.shell-file-upload-btn') && fileUploadInput) {
    win.querySelector('.shell-file-upload-btn').addEventListener('click', function() { fileUploadInput.click(); });
    fileUploadInput.addEventListener('change', function() {
      var file = this.files && this.files[0];
      if (!file) return;
      var path = (filePathInput.value.trim().replace(/\/+$/, '') || '') + '/' + file.name;
      fetch('/api/sessions/' + encodeURIComponent(sessionId) + '/files/upload?path=' + encodeURIComponent(path), {method:'POST',body:file})
        .then(function(r) { if (!r.ok) throw new Error(r.status); return r.json(); })
        .then(function() { shellFileBrowse(); })
        .catch(function(e) { console.error(e); });
    });
  }

  /* Exposed so reapplyWindowLanguage can redraw this window's file list from the
     path it already shows (no navigation, no request beyond the listing). */
  win._shellFileBrowse = shellFileBrowse;

  requestAnimationFrame(function () {
    var ch = win._activeChannelSid && win._channels && win._channels[win._activeChannelSid];
    if (ch && ch.term) try { ch.term.focus(); } catch (e1) {}
  });

  /* Mount the timeline rail (timeline.js) over this window's terminal. Last
     thing in this function on purpose: it reads win._activeChannelSid and the
     channel map, so those must exist first, and it is idempotent so the reused-
     window path that also calls this function keeps the rail it has. */
  initShellTimeline(win);
}

