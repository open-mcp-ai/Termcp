  var SHELL_WINDOW_PANELS_HTML =
    '<div class="shell-tab-panel shell-panel-fw" data-stab="fw" hidden>' +
      '<div class="shell-panel-head">' +
        '<span class="shell-panel-title" data-i18n="fw.panel.title">Port Forwardings</span>' +
        '<button type="button" class="shell-panel-icon-btn shell-fw-refresh-btn" title="Refresh" data-i18n-title="common.refresh"><svg viewBox="0 0 14 14" width="12" height="12" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M1.5 7A5.5 5.5 0 0111.3 3.8M12.5 7A5.5 5.5 0 012.7 10.2"/><path d="M12.5 1.5v2.5H10M1.5 12.5v-2.5H4"/></svg></button>' +
        '<button type="button" class="shell-panel-icon-btn shell-panel-icon-btn-accent shell-fw-add-btn" title="Add forwarding" data-i18n-title="fw.add"><svg viewBox="0 0 14 14" width="12" height="12" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M7 3v8M3 7h8"/></svg></button>' +
      '</div>' +
      '<div class="shell-panel-scroll shell-fw-list" data-i18n="common.loading">Loading...</div>' +
    '</div>' +
    '<div class="shell-tab-panel shell-panel-file" data-stab="file" hidden>' +
      '<div class="shell-panel-title shell-panel-file-title" data-i18n="win.files.tab">Files</div>' +
      '<div class="shell-file-toolbar">' +
        '<button type="button" class="shell-file-toolbar-btn shell-file-up" title="Parent" data-i18n-title="file.parent"><svg viewBox="0 0 14 14" width="13" height="13" aria-hidden="true"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" d="M2 10l5-5 5 5"/></svg></button>' +
        '<input type="text" class="shell-file-path" placeholder="/">' +
        '<input type="file" class="shell-file-upload-input" hidden>' +
        '<button type="button" class="shell-file-toolbar-btn shell-file-upload-btn" title="Upload" data-i18n-title="common.upload"><svg viewBox="0 0 14 14" width="13" height="13" aria-hidden="true"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" d="M7 2v8M4 5.5l3-3 3 3M2 10v1.5a1 1 0 001 1h8a1 1 0 001-1V10"/></svg></button>' +
        '<button type="button" class="shell-file-toolbar-btn shell-file-go" title="Load" data-i18n-title="file.load"><svg viewBox="0 0 14 14" width="13" height="13" aria-hidden="true"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" d="M1 7a6 6 0 0111.2-3.8M13 7a6 6 0 01-11.2 3.8"/><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" d="M13 2v2.5h-2.5M1 12v-2.5h2.5"/></svg></button>' +
      '</div>' +
      '<div class="shell-panel-scroll shell-file-listing"></div>' +
      '<div class="shell-file-ctx-menu" hidden><div class="file-ctx-item" data-action="download" data-i18n="common.download">Download</div><div class="file-ctx-item" data-action="rename" data-i18n="common.rename">Rename</div><div class="file-ctx-item file-ctx-danger" data-action="delete" data-i18n="common.delete">Delete</div></div>' +
    '</div>'; // end SHELL_WINDOW_PANELS_HTML
