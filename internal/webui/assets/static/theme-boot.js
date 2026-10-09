/* Runs before the body is parsed; theme.js owns subsequent user choices. */
var termcpBootRoot = document.documentElement;
var termcpBootLink = document.getElementById('termcp-theme');
var termcpBootID = 'default-light';
try {
  var termcpStoredTheme = localStorage.getItem('termcp.theme');
  if (termcpStoredTheme && termcpStoredTheme.charAt(0) !== '.' && !/[\/\\:\x00-\x1f\x7f]/.test(termcpStoredTheme)) termcpBootID = termcpStoredTheme;
} catch (termcpStorageError) {}
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
