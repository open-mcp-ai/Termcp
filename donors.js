/* Donor names come from donors.json beside this page, so the list grows
   without a markup edit. Entries are {name, kind?, url?, note?} — a bare
   string works too, and defaults to a human donor. `kind: "agent"` marks an
   AI maintainer; `note` is the hover hint saying what they did. Only the
   first 100 are shown. Amounts are deliberately not shown. */
let donorData = null;

function renderDonors() {
  const box = document.getElementById('donor-list');
  if (!box) return;
  const list = (Array.isArray(donorData) ? donorData : []).slice(0, 100);
  box.textContent = '';
  if (!list.length) {
    const empty = document.createElement('span');
    empty.className = 'donors-empty';
    empty.textContent = t('donors_empty');
    box.appendChild(empty);
    return;
  }
  list.forEach(entry => {
    const donor = typeof entry === 'string' ? { name: entry } : (entry || {});
    if (!donor.name) return;
    const kind = donor.kind === 'agent' ? 'agent' : 'human';
    /* The chip is a link whenever there is somewhere to go: an explicit url,
       else the GitHub handle a donor is named after. */
    const href = donor.url || (/^[A-Za-z0-9-]+$/.test(donor.name) ? 'https://github.com/' + donor.name : null);
    const el = document.createElement(href ? 'a' : 'span');
    el.className = 'donor';
    el.setAttribute('data-kind', kind);
    if (href) {
      el.href = href;
      el.target = '_blank';
      el.rel = 'noopener';
    }
    /* Hover hint: what this person or agent actually did, from donors.json.
       Falls back to the generic line when the entry carries no note. */
    el.title = donor.note || t('donor_hint');
    /* An icon, not a word: the same shape the transcript uses for the same
       kind of line. Role is still exposed as text for screen readers. */
    const kindTag = document.createElement('span');
    kindTag.className = 'donor-kind';
    kindTag.setAttribute('role', 'img');
    kindTag.setAttribute('aria-label', t(kind === 'agent' ? 'donor_agent' : 'donor_human'));
    kindTag.innerHTML = kind === 'agent'
      ? '<svg viewBox="0 0 24 24"><rect x="4" y="7" width="16" height="12" rx="2"/><path d="M12 7V4M9 12v2M15 12v2M2 11v4M22 11v4"/></svg>'
      : '<svg viewBox="0 0 24 24"><circle cx="12" cy="8" r="3.5"/><path d="M5 20a7 7 0 0 1 14 0"/></svg>';
    const nameTag = document.createElement('span');
    nameTag.className = 'donor-name';
    nameTag.textContent = donor.name;
    el.appendChild(kindTag);
    el.appendChild(nameTag);
    box.appendChild(el);
  });
}

async function fetchDonors() {
  try {
    /* no-store: the file edits on its own schedule, so neither the browser
       nor an intermediate cache may serve a stale list. */
    const res = await fetch('donors.json', { cache: 'no-store' });
    if (!res.ok) return;
    const data = await res.json();
    donorData = Array.isArray(data) ? data : null;
  } catch (err) {
    // Offline or opened over file:// — the section falls back to its empty state.
  }
  renderDonors();
}
