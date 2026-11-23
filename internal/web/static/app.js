(() => {
  'use strict';

  const $ = (id) => document.getElementById(id);
  const form = $('search-form');
  const fields = {
    q: $('q'), from: $('from'), since: $('since'), until: $('until'),
    folder: $('folder'), sort: $('sort'), attach: $('attach'),
  };
  let page = 1;
  let searchToken = 0; // a slow answer must not overwrite a newer one

  // ---- helpers ----

  async function api(path) {
    const r = await fetch(path, { headers: { Accept: 'application/json' } });
    const body = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(body.error || r.statusText);
    return body;
  }

  function el(tag, className, text) {
    const e = document.createElement(tag);
    if (className) e.className = className;
    if (text !== undefined) e.textContent = text;
    return e;
  }

  function fmtDate(iso) {
    if (!iso) return '';
    const d = new Date(iso);
    return isNaN(d) ? '' : d.toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' });
  }

  function fmtDay(iso) {
    if (!iso) return '';
    const d = new Date(iso);
    return isNaN(d) ? '' : d.toLocaleDateString([], { dateStyle: 'medium' });
  }

  function fmtSize(n) {
    if (n < 1024) return n + ' B';
    if (n < 1024 * 1024) return (n / 1024).toFixed(0) + ' KB';
    if (n < 1024 * 1024 * 1024) return (n / 1024 / 1024).toFixed(1) + ' MB';
    return (n / 1024 / 1024 / 1024).toFixed(2) + ' GB';
  }

  // ---- the form and the address bar ----

  function readForm(pg) {
    const p = new URLSearchParams();
    const set = (k, v) => { if (v) p.set(k, v); };
    set('q', fields.q.value.trim());
    set('from', fields.from.value.trim());
    set('since', fields.since.value);
    set('until', fields.until.value);
    set('folder', fields.folder.value);
    if (fields.attach.checked) p.set('attach', '1');
    if (fields.sort.value === 'date') p.set('sort', 'date');
    if (pg > 1) p.set('page', String(pg));
    return p;
  }

  function applyParams(p) {
    fields.q.value = p.get('q') || '';
    fields.from.value = p.get('from') || '';
    fields.since.value = p.get('since') || '';
    fields.until.value = p.get('until') || '';
    fields.attach.checked = p.get('attach') === '1';
    fields.sort.value = p.get('sort') === 'date' ? 'date' : 'relevance';
    page = Math.max(1, parseInt(p.get('page') || '1', 10) || 1);
    return p.get('folder') || '';
  }

  // ---- search ----

  async function runSearch(pg) {
    page = pg;
    const params = readForm(pg);
    const token = ++searchToken;
    history.replaceState(null, '', '?' + params.toString());
    $('summary').textContent = 'Searching...';
    try {
      const data = await api('/api/search?' + params.toString());
      if (token !== searchToken) return;
      render(data, params.get('q'));
    } catch (e) {
      if (token !== searchToken) return;
      $('results').replaceChildren();
      $('pager').hidden = true;
      $('summary').textContent = 'Error: ' + e.message;
    }
  }

  function render(data, query) {
    const list = $('results');
    list.replaceChildren();
    for (const h of data.hits) {
      const li = el('li', 'result');
      const a = el('a', 'subject', h.subject || '(no subject)');
      a.href = '#m' + h.id;
      a.addEventListener('click', (ev) => { ev.preventDefault(); openMessage(h.id); });
      li.append(a);

      const who = h.sender_name || h.sender_email || '(unknown sender)';
      const parts = [who, fmtDate(h.received_at), h.folder];
      if (h.has_attachments) parts.push('attachment');
      li.append(el('div', 'line', parts.filter(Boolean).join('  ·  ')));

      const snip = el('p', 'snippet');
      snip.innerHTML = h.snippet_html; // escaped by the server; only <mark> is added
      li.append(snip);
      list.append(li);
    }

    const n = data.hits.length;
    let text;
    if (n === 0) {
      text = query
        ? 'No messages found. Words such as "the" or "after" are ignored by the search.'
        : 'No messages in the index yet. Run: maillens sync';
    } else {
      text = n + ' message' + (n === 1 ? '' : 's') + ' on page ' + data.page + '  ·  ' + data.took_ms + ' ms';
      if (data.truncated) {
        text += '  ·  ranked among the newest matches only: narrow the query, or sort by newest to see older mail';
      }
    }
    $('summary').textContent = text;

    $('pager').hidden = !(data.page > 1 || data.has_more);
    $('prev').disabled = data.page <= 1;
    $('next').disabled = !data.has_more;
    $('pageno').textContent = 'Page ' + data.page;
  }

  // ---- one message ----

  async function openMessage(id) {
    try {
      const m = await api('/api/message/' + id);
      $('v-subject').textContent = m.subject || '(no subject)';
      const meta = $('v-meta');
      meta.replaceChildren();
      const add = (k, v) => { if (v) { meta.append(el('dt', '', k), el('dd', '', v)); } };
      const sender = m.sender_name && m.sender_email ? m.sender_name + ' <' + m.sender_email + '>' : (m.sender_name || m.sender_email);
      add('From', sender);
      add('To', m.to);
      add('Cc', m.cc);
      add('Date', fmtDate(m.received_at));
      add('Folder', m.folder);
      add('Attachments', m.attachments || (m.has_attachments ? '(names not available)' : ''));
      add('Size', fmtSize(m.size_bytes));
      const note = $('v-note');
      note.hidden = true;
      if (m.fetch_error) {
        note.textContent = 'The text of this message could not be read from Outlook: ' + m.fetch_error;
        note.hidden = false;
      } else if (m.body_cut) {
        note.textContent = 'The body is shown cut: it is longer than the limit of the index or of this view.';
        note.hidden = false;
      }
      $('v-body').textContent = m.body;
      const open = $('open-outlook');
      open.hidden = !m.outlook_url;
      if (m.outlook_url) open.href = m.outlook_url;
      const v = $('viewer');
      v.hidden = false;
      v.scrollTop = 0;
      $('close-viewer').focus();
    } catch (e) {
      $('summary').textContent = 'Error: ' + e.message;
    }
  }

  function closeViewer() { $('viewer').hidden = true; }

  // ---- the footer and the folder list ----

  async function loadStatus() {
    try {
      const s = await api('/api/status');
      const parts = [s.messages.toLocaleString() + ' messages indexed'];
      if (s.oldest && s.newest) parts.push(fmtDay(s.oldest) + ' to ' + fmtDay(s.newest));
      parts.push(fmtSize(s.db_size_bytes) + ' in the database');
      if (s.last_run) {
        const r = s.last_run;
        let t = 'last sync ' + fmtDate(r.finished_at) + ' (+' + r.added + ' -' + r.deleted + ')';
        if (r.failed) t += ', ' + r.failed + ' unreadable';
        if (r.note) t += ' — ' + r.note;
        parts.push(t);
      } else {
        parts.push('no sync yet');
      }
      $('status').textContent = parts.join('  ·  ');
    } catch (e) {
      $('status').textContent = 'Status is not available: ' + e.message;
    }
  }

  async function loadFolders(selected) {
    try {
      const folders = await api('/api/folders');
      const sel = fields.folder;
      for (const f of folders) {
        const o = document.createElement('option');
        o.value = f.path;
        o.textContent = f.path + ' (' + f.count.toLocaleString() + ')';
        sel.append(o);
      }
      sel.value = selected;
      if (sel.value !== selected) sel.value = '';
    } catch (e) { /* the folder list is a convenience */ }
  }

  // ---- wiring ----

  form.addEventListener('submit', (ev) => { ev.preventDefault(); runSearch(1); });
  $('prev').addEventListener('click', () => { runSearch(page - 1); window.scrollTo(0, 0); });
  $('next').addEventListener('click', () => { runSearch(page + 1); window.scrollTo(0, 0); });
  $('close-viewer').addEventListener('click', closeViewer);
  for (const k of ['folder', 'sort', 'attach', 'since', 'until']) {
    fields[k].addEventListener('change', () => runSearch(1));
  }
  document.addEventListener('keydown', (ev) => {
    if (ev.key === 'Escape') closeViewer();
    const typing = /^(INPUT|SELECT|TEXTAREA)$/.test(document.activeElement.tagName);
    if (ev.key === '/' && !typing) { ev.preventDefault(); fields.q.focus(); }
  });

  const initial = new URLSearchParams(location.search);
  const folder = applyParams(initial);
  if (['from', 'since', 'until', 'folder', 'attach'].some((k) => initial.get(k))) $('filters').open = true;
  loadFolders(folder).then(() => runSearch(page));
  loadStatus();
})();
