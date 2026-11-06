/* The archive's batch bar, from the SHARED component the hosts and extension
 * columns use (batch-bar.js). It is the same row of keys — the list-checks
 * switch, the invert key, and a split key whose wide half runs the column's verb
 * while its caret holds export and delete — because it would otherwise be a
 * third hand-written variant of the same control, and the first two already
 * drifted from each other once.
 *
 * The verb differs, and that is the only thing that differs: an archived session
 * cannot be dialled, so its split key opens TRANSCRIPTS read-only, which is the
 * one opening an over session has.
 *
 * `_sessionRegions` survives as a one-element list because the delete/‌prune paths
 * in conn-form.js walk it to drop ids that have left the registry. It no longer
 * owns the selection — the component does — so the only field still read is
 * `ids`, kept in step by the callbacks below.
 */
/* `gridId` is still what the renderer mounts into: the region table is what
 *  says which half of the snapshot this plate draws and where. Only the SELECTION
 *  moved into the component above (it owns the set and the keys), so the entry
 *  keeps the fields the renderer reads and drops the ids of the keys it no longer
 *  drives. */
var _sessionRegions = [{ dead: true, ids: new Set(), gridId: 'archive-grid' }];

/* THE ARCHIVE FILTER, kept here rather than on the input.
 *
 * It has to outlive a repaint: the archive is rebuilt from scratch on every
 * session snapshot (a session ending, a rename, another tab's delete), and an
 * input's own value would be thrown away with the row it was typed above. Holding
 * the query in a module variable is what makes a filter survive the push that
 * happens while someone is reading the result. */
var _archiveFilter = '';

/* archiveRowMatches reports whether an archived session answers the filter.
 *
 * IT MATCHES EVERY FACT THE ROW SHOWS, not just the name. The reason someone
 * reaches for this box is that two sessions look alike — the same profile dialled
 * twice, the same command run against two hosts — so matching only the name would
 * answer "everything" exactly when the archive is hardest to read. The host, the
 * command and the id are on the row for the same reason, so they are searchable
 * for the same reason. */
function archiveRowMatches(s, needle) {
  if (!needle) return true;
  var parts = [sessionCardName(s), s.ssh_config, s.ssh_endpoint, s.command, s.id, s.reason];
  if (s.shells && s.shells.length && typeof s.shells[0].exit_code === 'number') {
    parts.push('exit ' + s.shells[0].exit_code);
  }
  for (var i = 0; i < parts.length; i++) {
    var v = parts[i];
    if (v && String(v).toLowerCase().indexOf(needle) >= 0) return true;
  }
  return false;
}

/* ensureArchiveSearch puts the filter box above the ledger, once.
 *
 * IT IS BUILT HERE RATHER THAN IN index.html, and that is a budget decision, not
 * a style one: index.html is capped by a test and has a handful of bytes left, so
 * a new element in the document would fail the build.
 *
 * The box itself comes from ensureGridSearch (util.js), which hosts and
 * extensions use too: three columns that filter the same way must not carry three
 * copies of the same node, or the hooks and the caret handling drift apart. This
 * function only names the archive's own storage slot, catalog key and repaint. */
function ensureArchiveSearch(grid) {
  ensureGridSearch(grid, '_archiveFilter', 'archive.search.placeholder', t('archive.search.placeholder'), function () {
    renderSessionGrid('');
  });
}

/* The ticked sessions, in the order the grid shows them.
 *
 *  The component keys cards by whatever `nameAttr` names, and this column hands it
 *  the session ID rather than the displayed name. That is the fix for a real
 *  mis-selection: two archived sessions can share a name (a re-dialled profile
 *  produces `ck-main` twice), and a name-keyed set cannot tell them apart, so
 *  ticking one ticked both. Worse, an id->record map built by name silently kept
 *  only the LAST of a duplicated name, so a delete or an export acted on a record
 *  the user never pointed at.
 *
 *  A name is a label. It is not unique, and the archive holds records, not labels. */
function archivedSessionsFor(ids) {
  var snap = sessionsWithPending();
  var byId = {};
  (snap || []).forEach(function (s) {
    if (!s || !s.id || !isDeadSession(s)) return;
    byId[s.id] = s;
  });
  return (ids || []).map(function (id) { return byId[id]; }).filter(Boolean);
}

/** Export the selected transcripts as one text file.
 *
 *  There is no export route for a session: the backend composes TOML for
 *  CREDENTIALS (connections, extensions), and a transcript is neither — it is
 *  the byte log that already exists on disk, exposed through the same reads the
 *  viewer uses. So the file is assembled here from `GET /api/sessions/{id}/lines`,
 *  which is the line-addressed twin of the reader's own path, and each transcript
 *  is preceded by a header naming the session it came from.
 *
 *  Why a plain .txt and not TOML: this is a log. Wrapping it in a config format
 *  would invite someone to edit it and hand it back, and nothing here reads it.
 */
function exportArchivedTranscripts(sessions) {
  if (!sessions || !sessions.length) return;
  /* A ZIP of FOLDERS, one per session, one FILE per shell inside it.
   *
   * The old export concatenated every transcript into one .txt, which threw away the
   * one structure the archive actually has: a session is a container and the shells
   * are what ran inside it. A single blob made a 40-shell archive unreadable — you
   * could not tell where one shell stopped and the next started, and there was no
   * way to open just the channel you cared about.
   *
   * So: `termcp-archive-<stamp>.zip`, and inside it
   *
   *     <session>/
   *         session.txt      the record: name, id, profile, command, times, outcome
   *         shell-1-<id>.txt one per channel, in the order the server lists them
   *         shell-2-<id>.txt
   *
   * Written by hand rather than with a library: this page has no bundler and no
   * dependency graph, and a ZIP of uncompressed members is a few dozen lines of
   * DataView work. STORE (method 0) rather than DEFLATE is deliberate — a transcript
   * is already mostly printable text, the compression ratio would be modest, and
   * deflate would mean shipping an implementation of it. The format is its own
   * archive: any unzip reads it.
   */
  var files = [];   // { path, bytes }
  var failed = [];
  var chain = Promise.resolve();
  /* Sequential, not Promise.all: these are the same single-threaded handlers the page
     uses for live output, so firing forty at once would queue behind each other
     anyway while losing the archive's order. */
  sessions.forEach(function (s) {
    chain = chain.then(function () {
      var folder = safeZipSegment(sessionCardName(s));
      var stamp = function (ts) {
        var t0 = Number(ts) || 0;
        if (!t0) return '';
        if (t0 < 1e12) t0 *= 1000;
        try { return new Date(t0).toISOString(); } catch (e) { return ''; }
      };
      var record = [
        'name:     ' + sessionCardName(s),
        'id:       ' + s.id,
        'profile:  ' + (s.ssh_config || s.ssh_endpoint || ''),
        'command:  ' + (s.command || ''),
        'status:   ' + (s.status || ''),
        'created:  ' + stamp(s.created_at),
        'updated:  ' + stamp(s.updated_at),
        'lived:    ' + archiveDuration(s.created_at, s.updated_at),
        ''
      ].join('\n');
      return fetch(apiPath('/api/sessions/' + encodeURIComponent(s.id) + '/shells'))
        .then(function (r) { return r.ok ? r.json() : { shells: [] }; })
        .then(function (j) {
          var shells = (j && j.shells) || [];
          record += 'shells:   ' + shells.length + '\n';
          files.push({ path: folder + '/session.txt', text: record });
          /* One request per shell, in list order, each chained so the archive's order
             survives and the transcript bodies arrive attached to the shell they came
             from rather than to whatever finished first. */
          var inner = Promise.resolve();
          shells.forEach(function (sh, i) {
            var sid = sh && sh.id;
            if (!sid) return;
            inner = inner.then(function () {
              /* Built before the fetch so the failure path can still write a file that
                 says which shell it failed for — a bare "(output unavailable)" with no
                 identity is worse than nothing, because the folder then looks like it
                 lost a channel rather than like one read failed. */
              var head = [
                '# shell ' + (i + 1) + ' of ' + shells.length,
                '# id:      ' + sid,
                '# name:    ' + ((sh && sh.name) || ''),
                '# command: ' + ((sh && sh.command) || ''),
                '# mode:    ' + ((sh && sh.mode) || ''),
                '# status:  ' + ((sh && sh.status) || ''),
                '# exit:    ' + (sh && typeof sh.exit_code === 'number' ? sh.exit_code : ''),
                ''
              ].join('\n');
              var path = folder + '/shell-' + (i + 1) + '-' + safeZipSegment(String(sid)) + '.txt';
              return fetchAllShellOutput(sid)
                .then(function (body) {
                  files.push({ path: path, text: head + body + '\n' });
                })
                .catch(function () { files.push({ path: path, text: head + '(output unavailable)\n' }); });
            });
          });
          return inner.then(function () {
            /* A session whose shells the server no longer lists (older records, or a
               purge that kept the row) still gets its single transcript, so the export
               never silently drops a ticked session. */
            if (!shells.length) {
              return fetchAllSessionLines(s.id)
                .then(function (body) {
                  files.push({ path: folder + '/session-output.txt', text: body });
                });
            }
          });
        })
        .catch(function () { failed.push(sessionCardName(s)); });
    });
  });
  chain.then(function () {
    if (!files.length) {
      showCopyToast(t('archive.export.empty'));
      return;
    }
    var blob = zipStore(files);
    var url = URL.createObjectURL(blob);
    var link = document.createElement('a');
    link.href = url;
    var d = new Date();
    var p2 = function (n) { return (n < 10 ? '0' : '') + n; };
    link.download = 'termcp-archive-' + d.getFullYear() + p2(d.getMonth() + 1) + p2(d.getDate()) +
      '-' + p2(d.getHours()) + p2(d.getMinutes()) + '.zip';
    document.body.appendChild(link);
    link.click();
    link.remove();
    /* Revoke on the next task: revoking synchronously after click() races the
       browser's own fetch of the blob URL in some builds, and the download then
       arrives empty. */
    setTimeout(function () { URL.revokeObjectURL(url); }, 4000);
    /* Says "archive", not "transcripts": the unit is now a session FOLDER holding its
       shells, and a count of files would be a number the user never asked about. */
    showCopyToast(t('archive.export.done', { count: sessions.length }));
  });
  chain.then(function () {
    /* A session that could not be read at all is named, and that report is a
       separate toast from the success one on purpose: folding it into "exported 6
       sessions" would claim six folders for a batch that produced five, and the
       missing one is exactly what a reader has to know about. `failed` was collected
       and never shown before this — a silent partial export is the worst outcome,
       because the archive looks complete. */
    if (failed.length) {
      showCopyToast(t('archive.export.partial', { count: failed.length }));
    }
  });
}

/** A path segment that is safe inside a ZIP: no separators, no traversal, bounded.
 *
 *  Session names are user-supplied and can contain anything, including `/` and `..`.
 *  A ZIP entry whose name walks upwards is a security problem for whoever extracts
 *  it, and a name with a slash silently creates a nested folder nobody asked for, so
 *  every segment goes through here. The fallback keeps the entry addressable instead
 *  of dropping it. */
function safeZipSegment(name) {
  var s = String(name == null ? '' : name).trim();
  s = s.replace(/[\\/:*?"<>|\u0000-\u001f]+/g, '_');   // separators, control chars
  s = s.replace(/^\.+/, '_');                          // leading dots: no traversal, no hidden
  s = s.replace(/[. ]+$/, '');                         // trailing dot/space: Windows strips them
  if (s.length > 64) s = s.slice(0, 64);
  return s || '_';
}

/** Read a shell's COMPLETE output, paging until the server says there is no more.
 *
 *  An export is a copy of the record, so a window is the wrong shape for it. The
 *  first version asked for `output-range?max=524288&tail=1` — one request, the LAST
 *  512 KiB — which is not a truncated export only if the shell never printed more
 *  than that. When it did, the folder silently held the tail of a transcript with
 *  nothing anywhere saying the beginning was gone: the worst kind of loss, because
 *  the archive looks complete.
 *
 *  `output-range` is already a paging route: it answers `start`/`end`/`total` beside
 *  the bytes, where `total` is the whole stream and `end` is where this window
 *  stopped. So the loop is "read from where the last window ended, until end reaches
 *  total". The cap per request (the server's hard max is 512 KiB) is a window size,
 *  not a limit on what can be exported.
 *
 *  Two details are load-bearing:
 *
 *  - `end` is the byte offset AFTER the window, so it is the next `start`. Using
 *    `start + length` instead would be wrong: the server may clamp or re-align the
 *    window, and the returned offsets are the truth about what was served.
 *  - a window that does not advance (end <= start) ends the loop. Without that guard
 *    a server-side clamp (start below the retained base offset, for instance) would
 *    make this spin forever, and a hung export button is worse than a short file.
 *
 *  The reply is a JSON envelope, not raw text: it is the terminal-replay route, so
 *  several `frames` may come back for one window. Writing `JSON.stringify` into the
 *  .txt would hand the reader escape sequences wrapped in an object literal; the
 *  bytes are collected out of every frame and joined, which is what a transcript IS.
 */
function fetchAllShellOutput(sid) {
  var WINDOW = 512 * 1024;   // the server's hard max; one request per chunk
  var parts = [];
  var start = 0;

  function frameBytes(j) {
    if (!j) return '';
    if (Array.isArray(j.frames)) {
      var s = '';
      j.frames.forEach(function (f) { s += (f && f.d) || ''; });
      return s;
    }
    if (typeof j.d === 'string') return j.d;
    if (typeof j.data === 'string') return j.data;
    return '';
  }

  function step() {
    return fetch(apiPath('/api/shells/' + encodeURIComponent(sid) + '/output-range?max=' + WINDOW + '&start=' + start))
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (j) {
        if (!j) return parts.join('');
        parts.push(frameBytes(j));
        var end = typeof j.end === 'number' ? j.end : start;
        var total = typeof j.total === 'number' ? j.total : end;
        /* No progress, or nothing left: done. `end >= total` is the normal exit;
           `end <= start` is the guard that keeps a clamped window from looping. */
        if (end <= start || end >= total) return parts.join('');
        start = end;
        return step();
      });
  }

  return step();
}

/** Read a session's complete output by LINES, for records whose shells the server
 *  no longer lists (older snapshots, or a purge that kept the row).
 *
 *  The previous version asked for `/lines?limit=100000`, and both halves of that
 *  were wrong: the route takes `from` + `count`, not `limit`, so the parameter was
 *  ignored and the default of 50 lines applied — a record exported as fifty lines.
 *  `count` is also capped server-side (1000), so completeness needs paging here
 *  too, not a bigger number.
 *
 *  `from` is 1-based and absolute, `total_lines` is the whole record, so the loop is
 *  "ask for the next page until it has handed over total_lines". A page that returns
 *  nothing ends it, so a server that clamps cannot spin.
 */
function fetchAllSessionLines(sid) {
  var PAGE = 1000;   // the server's hard max count
  var parts = [];
  var from = 1;

  function step() {
    return fetch(apiPath('/api/sessions/' + encodeURIComponent(sid) + '/lines?from=' + from +
      '&count=' + PAGE + '&content=1'))
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (j) {
        if (!j) return parts.join('');
        var lines = j.lines || [];
        if (!lines.length) return parts.join('');
        lines.forEach(function (l) {
          /* `content` is present because content=1 was asked for. The fallbacks keep
             an older reply shape readable rather than printing "[object Object]". */
          if (typeof l === 'string') parts.push(l);
          else if (l) parts.push(l.content != null ? l.content : ((l.text || l.data) || ''));
        });
        var totalLines = typeof j.total_lines === 'number' ? j.total_lines : 0;
        from += lines.length;
        if (from > totalLines) return parts.join('');
        return step();
      });
  }

  return step();
}

/** Build an uncompressed (STORE) ZIP from [{path, text}] and return a Blob.
 *
 *  Written against the format rather than a library because this page ships no
 *  bundler: a dependency here would mean a build step for one button. Every member is
 *  STORE (method 0) — see the caller for why — and the two things a reader actually
 *  needs are the local header before each payload and the central directory at the
 *  end with one entry per member.
 *
 *  UTF-8 filenames are flagged (bit 11 of the general-purpose flags), which is what
 *  makes a non-ASCII session name extract to its real name instead of mojibake.
 *  Timestamps are DOS time, fixed to now: an archive is a snapshot of *when it was
 *  taken*, and carrying each session's own mtime would require per-entry conversion
 *  for a detail no one reads. */
function zipStore(entries) {
  var enc = new TextEncoder();
  var chunks = [];
  var central = [];
  var offset = 0;
  var now = new Date();
  var dosTime = ((now.getHours() & 0x1f) << 11) | ((now.getMinutes() & 0x3f) << 5) | ((now.getSeconds() / 2) & 0x1f);
  var dosDate = (((now.getFullYear() - 1980) & 0x7f) << 9) | (((now.getMonth() + 1) & 0xf) << 5) | (now.getDate() & 0x1f);
  /* CRC-32, the one checksum ZIP requires. Table built once per call: a ZIP export is
     a one-off, so the 256-entry table costs nothing next to the fetches above it. */
  var table = (function () {
    var t = new Uint32Array(256);
    for (var n = 0; n < 256; n++) {
      var c = n;
      for (var k = 0; k < 8; k++) c = (c & 1) ? (0xEDB88320 ^ (c >>> 1)) : (c >>> 1);
      t[n] = c >>> 0;
    }
    return t;
  })();
  function crc32(bytes) {
    var c = 0xFFFFFFFF;
    for (var i = 0; i < bytes.length; i++) c = table[(c ^ bytes[i]) & 0xFF] ^ (c >>> 8);
    return (c ^ 0xFFFFFFFF) >>> 0;
  }
  entries.forEach(function (e) {
    var nameBytes = enc.encode(String(e.path || 'file'));
    var data = enc.encode(String(e.text || ''));
    var crc = crc32(data);
    var local = new DataView(new ArrayBuffer(30));
    local.setUint32(0, 0x04034b50, true);       // local file header signature
    local.setUint16(4, 20, true);               // version needed
    local.setUint16(6, 0x0800, true);           // flags: UTF-8 names
    local.setUint16(8, 0, true);                // method: STORE
    local.setUint16(10, dosTime, true);
    local.setUint16(12, dosDate, true);
    local.setUint32(14, crc, true);
    local.setUint32(18, data.length, true);     // compressed size
    local.setUint32(22, data.length, true);     // uncompressed size
    local.setUint16(26, nameBytes.length, true);
    local.setUint16(28, 0, true);               // extra field length
    chunks.push(new Uint8Array(local.buffer), nameBytes, data);
    var cd = new DataView(new ArrayBuffer(46));
    cd.setUint32(0, 0x02014b50, true);          // central directory signature
    cd.setUint16(4, 20, true);                  // version made by
    cd.setUint16(6, 20, true);                  // version needed
    cd.setUint16(8, 0x0800, true);              // flags: UTF-8
    cd.setUint16(10, 0, true);                  // STORE
    cd.setUint16(12, dosTime, true);
    cd.setUint16(14, dosDate, true);
    cd.setUint32(16, crc, true);
    cd.setUint32(20, data.length, true);
    cd.setUint32(24, data.length, true);
    cd.setUint16(28, nameBytes.length, true);
    cd.setUint16(30, 0, true);                  // extra
    cd.setUint16(32, 0, true);                  // comment
    cd.setUint16(34, 0, true);                  // disk number
    cd.setUint16(36, 0, true);                  // internal attrs
    cd.setUint32(38, 0, true);                  // external attrs
    cd.setUint32(42, offset, true);             // local header offset
    central.push(new Uint8Array(cd.buffer), nameBytes);
    offset += 30 + nameBytes.length + data.length;
  });
  var centralSize = central.reduce(function (n, c) { return n + c.length; }, 0);
  var end = new DataView(new ArrayBuffer(22));
  end.setUint32(0, 0x06054b50, true);           // end of central directory
  end.setUint16(8, entries.length, true);       // entries on this disk
  end.setUint16(10, entries.length, true);      // entries total
  end.setUint32(12, centralSize, true);
  end.setUint32(16, offset, true);              // central directory offset
  var all = chunks.concat(central, [new Uint8Array(end.buffer)]);
  return new Blob(all, { type: 'application/zip' });
}

/** Delete the selected archived sessions, after one confirmation.
 *
 *  This is the archive's only destructive key, and it erases transcripts for
 *  good, so it goes through the same confirm-and-batch machinery the hosts column
 *  uses: one dialog naming the count, then every id in parallel, then a report of
 *  what failed rather than a silent partial success.
 */
function deleteArchivedSessions(sessions) {
  if (!sessions || !sessions.length) return;
  confirmDialog({
    title: t('batch.del.dialog'),
    message: tCount('batch.del.msg.one', 'batch.del.msg.other', { count: sessions.length }),
    okText: t('batch.del.ok', { count: sessions.length }),
    danger: true
  }).then(function (ok) {
    if (!ok) return;
    var banner = document.getElementById('session-load-banner');
    if (banner) setLoadBanner(banner, tCount('banner.deleting.one', 'banner.deleting.other', { count: sessions.length }));
    Promise.all(sessions.map(function (s) {
      return fetch(apiPath('/api/sessions/') + encodeURIComponent(s.id), { method: 'DELETE' })
        .then(function (r) {
          if (r.ok || r.status === 204) return { ok: true, id: s.id };
          /* A 404 means the record is already gone (deleted by another tab/sweep);
             treat as success so a stale row does not pop up a failure toast. */
          if (r.status === 404) return { ok: true, id: s.id, gone: true };
          return r.json().then(function (er) {
            return { ok: false, id: s.id, code: er && er.code, error: (er && er.error) || 'HTTP ' + r.status };
          }).catch(function () { return { ok: false, id: s.id, error: 'HTTP ' + r.status }; });
        })
        .catch(function (e) { return { ok: false, id: s.id, error: String((e && e.message) || e) }; });
    })).then(function (results) {
      var failed = results.filter(function (r) { return !r.ok; });
      results.forEach(function (r) { if (r.ok) _sessionRegions.forEach(function (rg) { rg.ids.delete(r.id); }); });
      if (banner) setLoadBanner(banner, '');
      if (failed.length) {
        showCopyToast(t('toast.delete.failed', { msg: failed.map(function (f) { return f.error; }).join('; ') }));
      } else {
        showCopyToast(tCount('toast.session.deleted.one', 'toast.session.deleted.other', { count: sessions.length }));
      }
      renderSessionGrid('');
    });
  });
}

/** The name the batch component indexes an archived session by.
 *
 *  It must be stable for as long as the row exists and unique within the column:
 *  displaySessionShort falls back to a prefix of the id when the session has no
 *  name, so two unnamed sessions never collide, and a renamed one changes in the
 *  same frame its row does. */
function sessionCardName(s) {
  var n = String((s && s.name) || '').trim();
  if (n && n.indexOf('session-') !== 0) return n;
  return displaySessionShort(s);
}

var _archiveBatch = (typeof createBatchBar === 'function') ? createBatchBar({
  bodyId: 'extpanel-body',
  /* alwaysOn, and this column is the reason the option exists: the archive's rows
     are ALWAYS tickable, because nothing here needs a mode. A row click opens a
     transcript (the only direct action an over session has), and the tick target is
     the row's own checkbox — so there is no "click means connect" to protect from
     "click means tick", which is the whole job of the hosts column's switch. */
  alwaysOn: true,
  selectingHostId: 'extpanel-body',
  toggleId: null,
  countId: 'archive-toggle-count',
  invertId: 'archive-sel-invert',
  /* Flat keys, no split and no menu: three verbs, three buttons. */
  splitId: null, mainId: null, caretId: null, menuId: null,
  exportId: 'archive-batch-export',
  deleteId: 'archive-batch-delete',
  actionIds: ['archive-batch-export', 'archive-batch-delete'],
  gridId: 'archive-grid',
  cardSelector: '.archive-row',
  /* KEYED BY ID, not by the displayed name — see archivedSessionsFor. `nameAttr` is
     the attribute the component reads to know which card is which, and it does not
     have to be a name despite the option's name. */
  nameAttr: 'data-sid',
  selectKey: 'nethub.batch.select',
  names: function () {
    return (sessionsWithPending() || [])
      .filter(function (s) { return s && s.id && isDeadSession(s); })
      .map(function (s) { return s.id; });
  },
  render: function () { renderSessionGrid(''); },
  tipFor: function (name) { return name; },
  /* The two verbs appear only once something is ticked, exactly as the extension
     plate's do — the same rule for the same reason, so the two flat bars behave
     alike. */
  /* Everything that reads the selection is painted HERE, on every change, because
   this hook is the one place the component calls after it has repainted the rows.
   Two of these were painted at the end of renderArchiveRows instead, and that is
   not the same thing: a rebuild happens when the LIST changes, not when the
   SELECTION does. Invert therefore marked the rows (the component's own class) and
   left every native checkbox on its old value — the bar said "2 selected" above
   two visibly unticked boxes. */
  onUpdate: function (n) {
    /* The keys are PAINTED, never removed. They used to be `hidden` while nothing
       was ticked, which took the toolbar away exactly when a reader was deciding
       what to do with the ledger. `disabled` is the honest state: the key is still
       there, still says what it does, and is visibly not available yet. The
       component already flips `disabled` in paintActions; nothing here duplicates
       it. */
    var badge = document.getElementById('archive-toggle-count');
    if (badge) {
      badge.textContent = String(n);
      /* Zero is an absence, not a count: dim it and keep it in the flow so the
         row's right edge does not shift on the first tick. */
      badge.classList.toggle('is-empty', n === 0);
    }
    /* The checkbox is the row's visible state and CSS cannot set it, so it is
       written here from the component's set — the same source of truth the row
       class comes from. */
    var grid = document.getElementById('archive-grid');
    if (grid) grid.querySelectorAll('.archive-row').forEach(function (row) {
      var box = row.querySelector('.archive-row-tick');
      if (!box) return;
      var key = row.getAttribute('data-sid');
      box.checked = !!(key && _archiveBatch.has(key));
    });
  },
  onExport: function (names) { exportArchivedTranscripts(archivedSessionsFor(names)); },
  onDelete: function (names) { deleteArchivedSessions(archivedSessionsFor(names)); }
}) : null;

/** A session is over once it stops running; the registry retains it for the
 *  archive rather than dropping it. */
function isDeadSession(s) { return !(s && s.status === 'running'); }

/** Sessions a subsystem keeps for itself stay off both plates.
 *
 *  The extension layer holds one connection per host and serves its extensions
 *  through channels in it; that session is termcp's own infrastructure rather
 *  than something the user asked for, so it is reported in the extension column
 *  and nowhere else. The record carries the namespace, which is what makes the
 *  hiding survive a restart: a client-side list of ids would not.
 *
 *  A caller's namespace (ns:...) is NOT hidden: those are ordinary sessions an
 *  agent asked for, and the person looking at this page is the one who owns the
 *  instance. The tag is shown on the card instead, so it is visible whose work a
 *  session is. */
function isHiddenSession(s) { return !!(s && s.namespace && s.namespace.indexOf('ext') === 0); }

/** The namespace an agent claimed when it started a session, or '' for an
 *  ordinary one. Shown as a chip so two agents' sessions are told apart without
 *  opening either. */
function sessionClientName(s) {
  var n = (s && s.namespace) || '';
  return n.indexOf('ns:') === 0 ? n.slice(3) : '';
}

/* ---- sessions still being dialled ----
 *
 * A connect that is still in progress has no session yet: the server registers
 * one only when the dial succeeds, so it is absent from every snapshot and the
 * Sessions plate cannot show it. The only place a dial exists before that is the
 * placeholder window that was opened for it, and that window IS the dial — it
 * holds the AbortController whose abort is the only thing that can stop the
 * handshake server-side (see startSessionAndOpenShell).
 *
 * These synthesise a card for each placeholder so the connecting work is visible
 * in the plate and therefore selectable by the same select-all and batch actions
 * as everything else. The card carries a prefixed id rather than a real session
 * id: it must never be mistaken for something the server can look up, because
 * `terminate` and `DELETE` would both 404 on it — cancelling the dial means
 * aborting the request that made it, not calling an endpoint.
 */
var PENDING_ID_PREFIX = 'pending:';

/** The synthetic card id for a dial in progress, keyed by the profile it dials.
 *  One placeholder per profile exists at a time (startSessionAndOpenShell
 *  refuses a duplicate), so the profile name is a stable identity for the card:
 *  the same value across re-renders, which is what lets a selection survive
 *  the frequent session frames that repaint this grid. */
function pendingCardId(connName) { return PENDING_ID_PREFIX + (connName || ''); }

/** Is this card id one of the synthesised connecting cards? */
function isPendingCardId(id) {
  return typeof id === 'string' && id.indexOf(PENDING_ID_PREFIX) === 0;
}

/** Every dial in progress, as snapshot-shaped records so the plate renders them
 *  through the same card markup as a real session. `status` is 'running'
 *  because the connect IS in progress — the card belongs to the live plate, and
 *  it is what makes it land in a select-all. */
function pendingSessionRecords() {
  if (typeof allShellWins !== 'function') return [];
  var out = [];
  allShellWins().forEach(function (w) {
    if (!w || !w._placeholder) return;
    var conn = w._pendingConnName || '';
    out.push({
      id: pendingCardId(conn),
      name: conn || t('tab.connecting'),
      status: 'running',
      _pending: true,
      _pendingConn: conn
    });
  });
  return out;
}

/** The list every plate reads: the server's snapshot plus the dials the server
 *  cannot know about yet, minus the sessions a subsystem owns. One place composes
 *  them, so the running plate, the archive, both regions' membership and pruning
 *  all see the same list — a hidden session that only the renderer skipped would
 *  still be counted by "select all" and ticked by the batch bar. */
function sessionsWithPending() {
  return (window._lastSessionsSnapshot || []).filter(function (s) {
    return !isHiddenSession(s);
  }).concat(pendingSessionRecords());
}

/** Stop a dial in progress. This is the batch form of the placeholder window's
 *  own close button: aborting the request is what makes the server's context
 *  end and the handshake stop (Manager.Create rechecks the context before it
 *  registers anything, so an aborted dial never leaves a session behind).
 *
 *  Returns true when a placeholder was found and cancelled. */
function cancelPendingConn(connName) {
  if (typeof getPendingShellWindowForConn !== 'function') return false;
  var win = getPendingShellWindowForConn(connName);
  if (!win) return false;
  closeShellWindow(win);
  return true;
}

/** Cancel every dial currently in progress, returning how many were stopped.
 *  Used by both batch actions: a connecting card has no session to terminate or
 *  delete, so whichever action is chosen, cancelling is the whole effect. */
function cancelAllPendingConns() {
  var cancelled = 0;
  pendingSessionRecords().forEach(function (p) {
    if (cancelPendingConn(p._pendingConn)) cancelled++;
  });
  return cancelled;
}

/** Sessions belonging to a region, from the last snapshot, so a region only ever
 *  sees the half it renders. */
function sessionsForRegion(region, snapshot) {
  return (snapshot || []).filter(function (s) { return s && s.id && isDeadSession(s) === region.dead; });
}

/** Drop from both plates' selections every id that is absent from `present`
 *  (a map keyed by session id). One pass, because a session that leaves the
 *  registry must leave both sets: it is gone from both plates.
 *
 *  `present` must come from the composed list (sessionsWithPending), not the raw
 *  snapshot: a dial in progress is absent from the server's snapshot by
 *  definition, so pruning against that alone would clear its tick on the next
 *  repaint — the selection would not survive the frame that renders the card. */
function pruneSessionSelections(present) {
  if (!present) return;
  /* The component owns the selection now, and it indexes by NAME, so the prune has
     to translate: an id with no session left behind it maps to no name, and the
     component's own prune (syncCards(true)) drops anything the current list does
     not offer. Doing it here as well keeps `_sessionRegions[0].ids` — which the
     delete path in conn-form.js still walks — from holding a dead id. */
  _sessionRegions.forEach(function (region) {
    region.ids.forEach(function (id) { if (!present[id]) region.ids.delete(id); });
  });
  if (_archiveBatch) _archiveBatch.syncCards(true);
}

/** Repaint the batch bar from the current snapshot.
 *
 *  The keys themselves belong to the shared component now (batch-bar.js), which
 *  paints its own count, invert key and split menu; what is left here is the one
 *  piece the component cannot know: which ids still exist. A session that leaves
 *  the registry must leave the selection too, or the next delete would address an
 *  id the server no longer has.
 *
 *  `syncCards(false)` deliberately does NOT prune: the caller has just rendered
 *  from the live snapshot, and a prune against a list that has not arrived yet
 *  would throw away a tick someone just made.
 */
function updateSessionBatchBar() {
  if (!_archiveBatch) return;
  var live = {};
  (sessionsWithPending() || []).forEach(function (s) { if (s && s.id) live[s.id] = true; });
  pruneSessionSelections(live);
  _archiveBatch.update();
}

// The list lives in window._lastSessionsSnapshot, written by
// applySessionsSnapshot and read by every pass below; the banner message is the
// only thing a caller varies.
function renderSessionGrid(bannerMsg) {
  setLoadBanner(document.getElementById('session-load-banner'), bannerMsg);
  // `all` carries the dials in progress as well: a connecting card has to be
  // rendered by the same pass (and be selectable by the same controls) as a
  // real session, or select-all would reach only the sessions the server
  // already knows about.
  var all = sessionsWithPending();
  /* ONE renderer now. The session plate that drew cards for live sessions was
     removed (a window is the live session's own surface), which left this loop with
     a single reachable branch: `renderTileGrid` and its helpers were 275 lines no
     snapshot could reach. The region table still says which half of the snapshot
     this column draws, so the shape of the pass did not change — only the choice
     that no longer existed. */
  _sessionRegions.forEach(function (region) {
    renderArchiveRows(region, all);
  });
  var present = {};
  all.forEach(function (s) { if (s && s.id) present[s.id] = true; });
  pruneSessionSelections(present);
  updateSessionBatchBar();
}

/** A compact age for an archive row ("3h ago").
 *
 *  An archived session is judged by WHEN it happened far more than by its id, so
 *  the timestamp is the one piece of metadata the row carries. Anything older
 *  than a month is left to the tooltip: a row is 32px and a date string would
 *  push the name out for a number nobody reads at that distance. */
function archiveAge(ts) {
  var t0 = Number(ts) || 0;
  if (!t0) return '';
  // Server timestamps are Unix seconds in one path and milliseconds in another;
  // the magnitude is what tells them apart.
  if (t0 < 1e12) t0 *= 1000;
  var secs = Math.max(0, Math.round((Date.now() - t0) / 1000));
  if (secs < 90) return t('archive.age.now');
  if (secs < 5400) return t('archive.age.min', { n: Math.round(secs / 60) });
  if (secs < 172800) return t('archive.age.hour', { n: Math.round(secs / 3600) });
  return t('archive.age.day', { n: Math.round(secs / 86400) });
}

/** How long the session lived, for an archive row ("2h 14m").
 *
 *  The two timestamps are already on the record and their difference is the one
 *  number a person cannot compute by looking at two rows: "started 09:12, ended
 *  11:26" needs arithmetic, "2h 14m" does not. Unlike the age, this is a FACT
 *  about a finished session rather than a distance from now, so it never changes
 *  and needs no unit ladder — a duration reads the same at any remove.
 *
 *  Seconds-resolution data lands in the same millisecond field inconsistently (the
 *  age function has the same guard), so both are normalised before subtracting.
 *  A negative or zero span returns "" rather than "0s": it means the record does
 *  not actually carry a span, and a zero on screen would be a claim. */
function archiveDuration(from, to) {
  var a = Number(from) || 0, b = Number(to) || 0;
  if (!a || !b) return '';
  if (a < 1e12) a *= 1000;
  if (b < 1e12) b *= 1000;
  var secs = Math.round((b - a) / 1000);
  if (secs <= 0) return '';
  var h = Math.floor(secs / 3600), m = Math.round((secs % 3600) / 60);
  if (h > 0) return m > 0 ? h + 'h ' + m + 'm' : h + 'h';
  if (m > 0) return m + 'm';
  return secs + 's';
}

/** When a session left `running`, in milliseconds, for ordering the ledger.
 *
 *  The archive is read newest-first: the session someone wants back is almost
 *  always the one that just ended, and a ledger in map order makes them search the
 *  whole strip for it. `updated_at` is the moment of the last transition, which for
 *  a finished session is the moment it finished (the API has no `ended_at`); the
 *  magnitude guard is the same one archiveAge needs, because seconds-resolution
 *  values reach that field on the restore path. */
function archiveEndedAt(s) {
  var t0 = Number((s && (s.updated_at || s.created_at)) || 0);
  if (!t0) return 0;
  if (t0 < 1e12) t0 *= 1000;
  return t0;
}

/** Newest first, with the id as a tiebreak so two sessions that ended in the same
 *  millisecond do not swap places between frames. */
function compareArchiveRows(a, b) {
  var d = archiveEndedAt(b) - archiveEndedAt(a);
  if (d) return d;
  return String(b.id || '').localeCompare(String(a.id || ''));
}

/** Render the archive as ROWS, not cards.
 *
 *  The archive is a ledger: it answers "what already happened" and is read in
 *  passing, so the expensive card anatomy — a terminal glyph, a lamp, a lock, a
 *  copy control — is the wrong shape for it. What one row
 *  needs is enough to recognise the session (its name), enough to judge it (when
 *  it ended) and a way to act on it (tick, or open it read-only).
 *
 *  Selection is the same set the cards use, so select-all, the batch trash and
 *  the frame's own count all keep working without knowing the shape changed. */
function renderArchiveRows(region, all) {
  var grid = document.getElementById(region.gridId);
  if (!grid) return;
  ensureArchiveSearch(grid);
  grid.innerHTML = '';
  var rows = all.filter(function (s) { return s && s.id && isDeadSession(s) === region.dead; });
  /* THE FILTER IS APPLIED BEFORE THE SORT, so the two never disagree about what the
     ledger holds: sorting a filtered set and filtering a sorted one produce the same
     order, and doing it in this one place keeps every caller (a snapshot, a rename,
     a delete) filtered without each remembering to. */
  if (region.dead && _archiveFilter) {
    rows = rows.filter(function (s) { return archiveRowMatches(s, _archiveFilter); });
  }
  /* ListAll walks a sync.Map, so its order is random and reshuffles on every
     frame — a ledger whose rows move under the pointer is not a ledger. Sorted by
     the moment each session ended, newest first. */
  rows.sort(compareArchiveRows);
  if (rows.length === 0) {
    /* An empty archive says what it holds rather than showing a blank strip: the
       column is collapsed by default, so the one time it is opened with nothing
       in it is the one time the sentence has to carry its own meaning.

       A FILTER WITH NO HITS IS A DIFFERENT SENTENCE from an empty archive, and
       saying "nothing here" over a full archive would read as data loss. */
    var empty = document.createElement('div');
    empty.className = 'sess-plate-empty archive-empty';
    if (region.dead && _archiveFilter) {
      empty.textContent = t('archive.search.empty');
    } else {
      empty.textContent = window._lastSessionsSnapshot ? t('archive.empty.hint') : '';
    }
    grid.appendChild(empty);
    if (typeof paintArchiveRail === 'function') paintArchiveRail();
    return;
  }
  rows.forEach(function (s) {
    var sid = s.id || '';
    var label = sessionCardName(s);
    /* The server reports created_at / updated_at / exit_code and nothing else:
       there is no dedicated "ended_at", so the age of an archived session is the
       last time the record changed — which for a session that has exited is the
       moment it exited. Reading a field the API never sends would leave every
       row blank, and a blank age column is worse than no column. */
    var age = archiveAge(s.updated_at || s.created_at);

    var row = document.createElement('div');
    row.className = 'archive-row';
    row.setAttribute('data-sid', sid);
    /* The batch component indexes rows by NAME (it is shared with two card grids
       whose cards carry a name, not an id), so the row carries the same name it
       is selectable under. Ticking is the component's job from here: it adds
       .batch-selected and drives the count, exactly as it does on a host card. */
    /* The component keys this row by ID: two archived sessions may share a name. */
    row.setAttribute('data-sid', sid);
    row.title = sid + (s.reason ? ' · ' + reasonLabel(s.reason) : '');
    /* The checkbox is a real checkbox, and it is the ONLY tick target: the row
       itself always opens. That split is what lets this column skip a selection
       mode entirely — the hosts card cannot, because a card body click there must
       mean "connect" and a batch needs some other place to put the tick. A row has
       room for both, so it carries both. */
    /* The row is a LEDGER ENTRY: what it was, where it ran, how it ended, when.
     *
     * It carried a name and an age, which is the least a row can say and still be a
     * row. Everything else the record already holds — the profile it dialled, the
     * command, the shell's exit code, how long it lived — was thrown away, so two
     * sessions of the same name were indistinguishable and the only way to tell why
     * one had died was to open both transcripts. Those are the facts someone scans
     * an archive FOR, so they are on the row.
     *
     * Three parts, in the order the eye asks: identity (name, tick, id), then the
     * circumstances (profile, command, outcome), then the time (when, how long).
     * The erase button is last and quiet until hovered: it is the one irreversible
     * action on this page, so it must not compete with the row's open target. */
    var endpoint = s.ssh_config || s.ssh_endpoint || '';
    var cmd = s.command || '';
    /* The exit code lives on the SHELL, not the session: a session is a container
       and what exited is a process inside it. Reading s.exit_code yields null for
       every row (the server sends null there), which is why the column said nothing
       before. The first shell is the session's own. */
    var code = null;
    if (s.shells && s.shells.length) code = s.shells[0].exit_code;
    var outcome = '';
    if (typeof code === 'number') outcome = code === 0 ? 'exit 0' : 'exit ' + code;
    else if (s.reason) outcome = reasonLabel(s.reason);
    var lived = archiveDuration(s.created_at, s.updated_at);

    row.innerHTML =
      '<input type="checkbox" class="archive-row-tick" tabindex="-1" aria-hidden="true">' +
      '<span class="archive-row-main">' +
        '<span class="archive-row-name">' + escapeHtml(label) + '</span>' +
        '<span class="archive-row-meta">' +
          (endpoint ? '<span class="archive-row-chip">' + escapeHtml(endpoint) + '</span>' : '') +
          (cmd ? '<span class="archive-row-cmd">' + escapeHtml(cmd) + '</span>' : '') +
        '</span>' +
        /* The session id, on the row rather than only in the tooltip.
         *
         * The id is what every other surface speaks: `term`, the REST routes, an
         * `open` in a terminal, a locator pasted into a note. It was reachable only by
         * hovering a row and reading a tooltip, which is not a thing you can copy from
         * and not a thing you can see while comparing two rows — and two archived
         * sessions of the same name are exactly when the id is the only thing that
         * tells them apart.
         *
         * It is the third line of the identity column because that is where the space
         * already was: name and meta measured about 23px inside a 44px row, so the
         * lower half of the column was empty. Same monospace, dimmer, selectable. */
        '<span class="archive-row-id" title="' + escapeHtml(sid) + '">' + escapeHtml(sid) + '</span>' +
      '</span>' +
      '<span class="archive-row-facts">' +
        (outcome ? '<span class="archive-row-outcome' + (code ? ' is-failed' : '') + '">' + escapeHtml(outcome) + '</span>' : '') +
        '<span class="archive-row-age">' + escapeHtml(age) + '</span>' +
        (lived ? '<span class="archive-row-lived">' + escapeHtml(lived) + '</span>' : '') +
      '</span>' +
      '<button type="button" class="archive-row-del" title="Delete" data-i18n-title="common.delete" aria-label="Delete this archived session" data-i18n-aria="archive.aria.delete">' +
        '<svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M19 7l-.867 12.142A2 2 0 0116.138 21H7.862a2 2 0 01-1.995-1.858L5 7m5 4v6m4-6v6m1-10V4a1 1 0 01-1-1h-4a1 1 0 01-1 1v3M4 7h16"/></svg>' +
      '</button>';

    /* A row is one of two things depending on the mode, which is the contract the
       shared component expects of a card: untoggled it opens the transcript
       read-only (the only "open" an over session has), toggled it ticks. The
       decision is made from the component's own state rather than from a local
       flag, so the two cannot disagree about which mode the column is in. */
    row.setAttribute('role', 'button');
    row.setAttribute('tabindex', '0');
    row.setAttribute('aria-label', t('archive.aria.open', { name: label }));
    /* ALWAYS opens, never ticks: the tick lives on the checkbox, which is why
       this column needs no selection mode. */
    function activate() {
      clearSessNotified(sid);
      focusSessionWindow(s.name || '', sid, null, { readOnly: true });
    }
    row.addEventListener('click', function (e) {
      if (e.target && e.target.closest && e.target.closest('.archive-row-tick')) return;
      /* The erase key is the row's own control and must not fall through to the
         open target: without this guard, deleting a session would also open its
         transcript as the click bubbles. */
      if (e.target && e.target.closest && e.target.closest('.archive-row-del')) return;
      activate();
    });
    var del = row.querySelector('.archive-row-del');
    if (del) del.addEventListener('click', function (e) {
      e.preventDefault();
      e.stopPropagation();
      /* Same path the batch delete uses, one row wide: the confirmation, the API
         call and the unticking all live there, so a second implementation here
         would be a second set of rules for the one irreversible action. */
      /* It takes session RECORDS and deletes by id, so the row hands back the one
         it was built from rather than the name it displays. */
      if (typeof deleteArchivedSessions === 'function') deleteArchivedSessions([s]);
    });
    var tick = row.querySelector('.archive-row-tick');
    if (tick) tick.addEventListener('click', function (e) {
      /* The component owns the set, so it is told, not bypassed; it repaints the
         row's own class and the count in one pass. */
      e.stopPropagation();
      if (_archiveBatch) _archiveBatch.toggleSelection(sid, row);
    });
    row.addEventListener('keydown', function (e) {
      if (e.key !== 'Enter' && e.key !== ' ') return;
      e.preventDefault();
      activate();
    });
    grid.appendChild(row);
  });
  /* The rail's count is refreshed here rather than left to the observer that
     watches this grid: an observer fires on a later task, so the number would
     lag the list by a frame — visible as a rail that still reads the previous
     count right after a session dies. archive-column.js loads after this file,
     so the very first render can run before that declaration exists; the typeof
     check is what keeps the first paint from throwing instead of counting. */
  if (typeof paintArchiveRail === 'function') paintArchiveRail();
  /* Repaint the ticks from the component's set: rows are rebuilt on every frame,
     so a selection has to be re-applied to the new elements or it would appear to
     clear itself whenever a session died. The checkbox is painted here rather than
     by the component, because the component knows one class (`batch-selected`) and
     this row's visible state lives in a native input that CSS cannot set. */
  if (_archiveBatch) {
    _archiveBatch.syncCards(false);
    /* update() runs onUpdate, which paints the checkboxes and the key row. Doing
       any of that here too would be a second writer for one state. */
    _archiveBatch.update();
  }
}

/** Convert a live terminal window to read-only when its session becomes DEAD:
 *  stop streaming/input while keeping the ordinary xterm screen and tabs. Also
 *  surfaces a "dead" badge in the window title so the DEAD state is visible
 *  inside the terminal window (not only on the session tile). Idempotent. */
function lockWindowReadonly(win) {
  if (!win) return;
  win._lockedReadonly = true;
  win._readOnly = true;
  win._inputClosed = true;
  setWindowDeadBadge(win);
  var chans = win._channels || {};
  Object.keys(chans).forEach(function (sid) {
    var ch = chans[sid];
    sendTerminalWatch(sid, false);
    if (ch.term) {
      ch.streamDone = true;
      try { ch.term.options.readOnly = true; } catch (e) {}
      try { ch.term.readOnly = true; } catch (e) {}
    }
    // Channel went live→DEAD with its history already fully painted: render the end
    // marker here as well, so it shows even if the terminal_done frame was lost (e.g.
    // the WS dropped before the server emitted it). Freshly opened DEAD channels have
    // historyLoaded=false and get the marker from the bootstrap .finally instead, so
    // we never paint before their persisted output lands.
    if (ch.historyLoaded === true) showTerminalEndedMarker(win, ch);
  });
}

/** Insert (once) a compact "dead" badge into a terminal window's title right
 *  after the session-id group. Never disturbs layout — flex-shrink:0 and it
 *  sits inside the existing .shell-window-title flex row. */
function setWindowDeadBadge(win) {
  if (!win) return;
  /* The header lamp tracks the same transition: a window that just became
     read-only must not keep claiming a live link. */
  if (typeof setWindowLinkState === 'function') setWindowLinkState(win, 'is-dead');
  if (win._deadBadgeAdded) return;
  var title = win.querySelector('.shell-window-title');
  if (!title) return;
  win._deadBadgeAdded = true;
  var st = document.createElement('span');
  st.className = 'shell-dead-badge';
  /* i18n hooks, not just a painted string: the badge is created once per window,
     and the text it was born with was whatever language the page spoke then. The
     hooks let every later applyI18n pass — and the language switch's
     reapplyWindowLanguage — rewrite it, instead of the badge keeping the
     language of the moment it appeared (which read as a hard-coded string). */
  st.setAttribute('data-i18n', 'session.dead.badge');
  st.setAttribute('data-i18n-title', 'session.dead.badgeTitle');
  st.title = t('session.dead.badgeTitle');
  st.textContent = t('session.dead.badge');
  title.appendChild(st);
}

/** Render the terminal-end marker on one channel at most once.
 *
 *  The marker belongs IN the terminal — it reads as the last line of the session's
 *  own output, which is what it is — so it is written with term.writeln, exactly
 *  as it always was. What changed is that the buffer cells it produced are no
 *  longer treated as immutable: the printed line is recorded on the channel, and
 *  a language switch rewrites that one line in place (see reapplyEndedMarker).
 *  Without that, a user who opened an archived session and then switched languages
 *  saw "[会话已结束]" frozen in the old language, which read as a hardcoded string.
 *
 *  Idempotent per channel (gated on ch.endedMarkPrinted). */
function showTerminalEndedMarker(win, ch) {
  if (!ch || ch.endedMarkPrinted) return;
  ch.endedMarkPrinted = true;
  if (win && ch.tabEl) {
    /* The chip is repainted, not revealed: it carries a state, and "ended" is
       only one of them, so the node is always present and its text is written
       by whoever changes the state. */
    var sid = ch.tabEl.getAttribute('data-chsid');
    if (sid) shellStatusSet(win, sid, 'ended');
  }
  if (!ch.term) return;
  var text = t('term.ended');
  ch.endedMarkerText = text;
  try {
    ch.term.writeln('\r\n\x1b[33m' + text + '\x1b[0m', function () {
      shellTermScrollToBottomIfStuck(ch.term, true);
    });
  } catch (e) {
    try { ch.term.writeln('\r\n\x1b[33m' + text + '\x1b[0m'); } catch (e2) {}
    shellTermScrollToBottomIfStuck(ch.term, true);
  }
}

/** Rewrite the already-printed end marker in the language now in force.
 *
 *  The marker is a line in xterm's buffer, so this is the only way it can follow a
 *  language switch: the buffer is addressed directly rather than re-printed (a
 *  second writeln would leave the old line above the new one).
 *
 *  Two guards keep this from eating real output:
 *   - The marker must still be the LAST non-empty line. Output that arrived after
 *     it (a late frame) means the sentinel is no longer the tail, and overwriting
 *     whatever is at that row would corrupt the transcript.
 *   - That last line must still read as a previously printed marker. If it does not
 *     match any language's text, something else owns the row and is left alone.
 *
 *  The rewrite is ANSI: save cursor, jump to the marker's viewport row, erase the
 *  line, print the new text, restore cursor. Nothing else in the buffer moves. */
function reapplyEndedMarker(ch) {
  if (!ch || !ch.endedMarkPrinted || !ch.term) return false;
  var term = ch.term;
  var buf = term.buffer && term.buffer.active;
  if (!buf || typeof buf.getLine !== 'function') return false;
  var y = -1;
  for (var i = buf.length - 1; i >= 0; i--) {
    var line = buf.getLine(i);
    if (!line) continue;
    var str = line.translateToString(true);
    if (str && str.trim()) { y = i; break; }
  }
  if (y < 0) return false;
  var current = buf.getLine(y).translateToString(true).trim();
  /* Only a row that still holds a marker in SOME language may be rewritten. The
     catalog is the list of legitimate values; anything else is the session's own
     output and must not be touched. */
  if (!isEndedMarkerText(current)) return false;
  var next = t('term.ended');
  if (current === next) return true;
  var row = y - buf.baseY + 1;
  if (row < 1 || row > term.rows) return false;
  try {
    term.write('\x1b7\x1b[' + row + ';1H\x1b[2K\x1b[33m' + next + '\x1b[0m\x1b8');
  } catch (e) { return false; }
  ch.endedMarkerText = next;
  return true;
}

/** True when the string is the end marker in any language the catalog knows.
 *
 *  I18N_CATALOG is a plain script-level var (i18n-catalog.js loads before this),
 *  so the scan is direct; the t() fallback covers a future rename of that var. */
function isEndedMarkerText(str) {
  if (!str) return false;
  var langs = (typeof I18N_CATALOG === 'object' && I18N_CATALOG) || null;
  if (langs) {
    for (var lang in langs) {
      if (!Object.prototype.hasOwnProperty.call(langs, lang)) continue;
      var val = langs[lang] && langs[lang]['term.ended'];
      if (typeof val === 'string' && val.trim() === str) return true;
    }
  }
  return str === t('term.ended');
}

/**
 * toggleSessionApproval flips a session's approval gate.
 *
 * Turning it on asks for the threshold first: the number of approvers is the
 * real control (termcp authenticates a deployment with one token, so it cannot
 * prove who an approver is), and defaulting it silently to 1 would hide that.
 * Turning it off cancels everything pending server-side, hence the confirmation.
 */
function toggleSessionApproval(s, gated, btn) {
  if (gated) {
    confirmDialog({
      title: t('review.disable.title'),
      message: t('review.disable.msgSession', { session: displaySessionShort(s) }),
      okText: t('review.disable.ok')
    }).then(function (ok) {
      if (!ok) return;
      setSessionApproval(s.id, false).catch(function (e) {
        showCopyToast(t('review.toast.failed', { msg: String(e.message || e) }));
      });
    });
    return;
  }
  setSessionApproval(s.id, true).catch(function (e) {
    showCopyToast('Failed: ' + String(e.message || e));
  });
}

/** setSessionApproval PATCHes the gate. The session-list frame re-renders the
 *  card, so no local state is kept here — the server is the single source.
 *
 *  No reviewer count is sent: review has one reviewer, so a number would be a
 *  field with a single valid value.
 */
function setSessionApproval(sessionID, enabled) {
  var body = enabled ? { enabled: true } : { enabled: false };
  return fetch(sessionAPI(sessionID, '/approval'), {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body)
  }).then(function (r) {
    if (!r.ok) return r.text().then(function (t) { throw new Error(t || ('HTTP ' + r.status)); });
    return r.json();
  }).then(function (j) {
    showCopyToast(enabled ? t('review.toast.on') : t('review.toast.off'));
    // The server broadcasts the new session list (Manager.EnableApproval), and the
    // WebSocket frame re-renders the cards. Nothing is refreshed locally: reading
    // back here would be a second source of truth for the same state.
    return j;
  });
}

function applySessionsSnapshot(sessions) {
  window._lastSessionsSnapshot = sessions;
  renderSessionGrid('');
  /* The dock's collapsed row reports the sessions that EXIST, not only the
     windows that are open, so it has to be repainted from the same snapshot the
     plates are built from. Nothing else does it: the tab refresh runs on window
     open/close/raise, and a session started by an agent opens no window. */
  if (typeof refreshSessionTabbar === 'function') refreshSessionTabbar();
  loadForwards();
  // Reconcile open ordinary shell windows with the retained registry:
  //  - running parent → leave live
  //  - DEAD parent → lock readonly, keep the window and tabs
  //  - deleted parent → close the window
  var sessionById = {};
  (sessions || []).forEach(function (s) { if (s.id) sessionById[s.id] = s; });
  // Reconcile EVERY open window — floating (shellWindowsEl) AND tiled
  // (pane-grid). Iterating only the floating container left windows for
  // deleted sessions open forever in the tiled workspace; allShellWins()
  // is the single source of truth for "all open shell windows".
  var wins = allShellWins();
  for (var i = 0; i < wins.length; i++) {
    var w = wins[i];
    if (!w._parentSid || w._placeholder) continue;
    var s = sessionById[w._parentSid];
    if (!s) {
      releaseSessionUIState(w._parentSid);
      closeShellWindow(w);
      continue;
    }
    if (s.status !== 'running') lockWindowReadonly(w);
    // The title carries the session's name, and a rename can arrive from anywhere
    // (this window's own title, a dock tab's menu, an agent over MCP), so the
    // session frame is what repaints it. The window was opened from a LABEL, which
    // stops being the name the moment anyone renames the session.
    if (typeof paintWindowTitleName === 'function') paintWindowTitleName(w, s);
    // Approval mode is part of the session snapshot, so an open window reflects a
    // switch flipped in another tab without needing its own request.
    w._approvalNeed = s.approval_need || 1;
    applyApprovalMode(w, !!s.approval_mode, s.approval_scopes);
  }
  // Prune invisible client-side state for sessions that have left the
  // server registry entirely, even if no window was open when they died.
  pruneDeadSessionUIState(sessionById);
  // Panel grouping is per browser tab, not session data. The first snapshot is
  // the point where saved session/shell ids can be reconciled before rebuilding.
  if (typeof restorePanelLayout === 'function') restorePanelLayout(sessionById);
  refreshAllWindowTabs();
}

/** Drop client-side maps for sessions no longer present in sessionById. */
function pruneDeadSessionUIState(liveMap) {
  if (!liveMap) return;
  if (_uiNotifHighlights) {
    Object.keys(_uiNotifHighlights).forEach(function(id) { if (!liveMap[id]) delete _uiNotifHighlights[id]; });
  }
  pruneSessionSelections(liveMap);
  if (window._shellLastActive) {
    Object.keys(window._shellLastActive).forEach(function(id) { if (!liveMap[id]) delete window._shellLastActive[id]; });
  }
  if (window._pendingTerminalWatch) {
    Object.keys(window._pendingTerminalWatch).forEach(function(id) { if (!liveMap[id]) delete window._pendingTerminalWatch[id]; });
  }
}

/** Drop every client-side map keyed by a session id that no longer exists.
 *  Windows are the visible half of the leak; these maps are the invisible
 *  half (stale highlights, remembered tabs, queued watches). */
function releaseSessionUIState(sid) {
  if (!sid) return;
  if (_uiNotifHighlights) delete _uiNotifHighlights[sid];
  _sessionRegions.forEach(function (region) { region.ids.delete(sid); });
  if (window._shellLastActive) delete window._shellLastActive[sid];
  if (window._pendingTerminalWatch) delete window._pendingTerminalWatch[sid];
}

/** Per-window tab refresh: fetch child shells for win's parent session and sync tabs.
 *  Each window is independent — no shared loop state, no cross-contamination. */
function refreshWindowTabs(win) {
  if (!win || !win._parentSid) return;
  fetch(sessionAPI(win._parentSid, '/shells'))
    .then(function(r) { if (!r.ok) return; return r.json(); })
    .then(function(j) {
      if (!j || !j.shells) return;
      syncWindowTabs(win, j.shells);
    }).catch(function() {});
}

/** Notify all open windows that the global session list changed; each window independently
 *  decides whether to refresh its own channel tabs. Covers tiled panes too. */
function refreshAllWindowTabs() {
  var wins = allShellWins();
  for (var i = 0; i < wins.length; i++) {
    refreshWindowTabs(wins[i]);
  }
}

function syncWindowTabs(win, shells) {
  var existing = win._channels || {};
  if (win._readOnly) {
    // DEAD view: render every retained shell snapshot as a read-only tab. Never
    // prune on refresh (all snapshot shells report exited) and never send input.
    (shells || []).forEach(function(s) {
      var sid = s.shell_id || s.id;
      if (!existing[sid] && channelFitsWindow(win, sid)) createChannelTab(win, sid, { index: s.index || 0 });
    });
    return;
  }
  var running = shells.filter(function(s) { return s.status === 'running'; });
  var exited = shells.filter(function(s) { return s.status !== 'running'; });
  // Add tabs for shells the window does not show yet. Every panel of a session is a
  // peer, and so is every channel inside it: there is no "root" channel that this
  // rule treats differently. A channel the window already shows is skipped by
  // `existing`, and one another panel owns is refused by `channelFitsWindow` — those
  // two facts are the whole filter.
  exited.forEach(function(s) {
    var sid = s.shell_id || s.id;
    if (!existing[sid] && channelFitsWindow(win, sid)) createChannelTab(win, sid, { index: s.index || 0, readOnlyHistory: true });
  });
  running.forEach(function(s) {
    var sid = s.shell_id || s.id;
    if (!existing[sid] && channelFitsWindow(win, sid)) createChannelTab(win, sid, { index: s.index || 0 });
  });
  // Prune tabs for shells the server no longer lists at all — closed elsewhere
  // (MCP shell_close, another panel). A shell that merely exited stays listed with
  // its buffer, so its tab stays too. No channel is exempt: once the server stops
  // listing it, a tab for it would be a view of something that does not exist.
  Object.keys(existing).forEach(function(sid) {
    var found = shells.some(function(s) { return (s.shell_id || s.id) === sid; });
    if (!found && existing[sid]) closeChannelTab(win, sid);
  });
}

/* Language switch: re-render the tiles from the snapshot in memory (no request). */
onLangChange(function () { renderSessionGrid(''); });

