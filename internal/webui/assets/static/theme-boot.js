/* Runs before the body is parsed; theme.js owns subsequent user choices. */
var termcpBootRoot = document.documentElement;
var termcpBootLink = document.getElementById('termcp-theme');
/* The system's own light/dark preference: what "auto" resolves through, so the
   stored choice can stay 'auto' and follow the OS instead of freezing whichever
   theme the system happened to be in when it was picked. theme.js carries the
   same four lines, because this file runs in <head> — before theme.js exists —
   and has to resolve the choice before the first paint. */
function termcpSystemTheme() {
  try {
    return window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches ? 'default-dark' : 'default-light';
  } catch (e) { return 'default-light'; }
}
/* Unset means auto: a fresh browser follows its OS rather than being pinned to
   the light theme the <link> is hardcoded to. */
var termcpBootChoice = 'auto';
try {
  var termcpStoredTheme = localStorage.getItem('termcp.theme');
  if (termcpStoredTheme && termcpStoredTheme.charAt(0) !== '.' && !/[\/\\:\x00-\x1f\x7f]/.test(termcpStoredTheme)) termcpBootChoice = termcpStoredTheme;
} catch (termcpStorageError) {}
var termcpBootID = termcpBootChoice === 'auto' ? termcpSystemTheme() : termcpBootChoice;
termcpBootRoot.setAttribute('data-theme', termcpBootID);
function termcpBootFinish() {
  clearTimeout(termcpBootTimer);
  termcpBootRoot.classList.remove('theme-loading');
  document.dispatchEvent(new CustomEvent('termcp:themechange', { detail: termcpBootID }));
}
function termcpBootFallback() {
  if (termcpBootID !== 'default-light') {
    termcpBootID = 'default-light';
    termcpBootRoot.setAttribute('data-theme', termcpBootID);
    clearTimeout(termcpBootTimer);
    termcpBootTimer = setTimeout(termcpBootFinish, 10000);
    termcpBootLink.href = 'themes/default-light/theme.css';
  } else {
    termcpBootFinish();
  }
}
var termcpBootTimer;
if (termcpBootID !== 'default-light') {
  termcpBootRoot.classList.add('theme-loading');
  termcpBootLink.onload = termcpBootFinish;
  termcpBootLink.onerror = termcpBootFallback;
  termcpBootTimer = setTimeout(termcpBootFallback, 10000);
  termcpBootLink.href = 'themes/' + encodeURIComponent(termcpBootID) + '/theme.css';
}
