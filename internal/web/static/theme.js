// Applies the saved (or system) theme before the first paint.
//
// This is a separate file, loaded synchronously in <head>, for two reasons: the
// CSP forbids inline scripts, and a deferred script would run after the browser
// has already painted the default palette — which is exactly the flash of wrong
// colours this exists to prevent.
(function () {
  var KEY = 'dumpster-theme';
  var saved = null;
  try { saved = window.localStorage.getItem(KEY); } catch (e) { /* private mode */ }
  var prefersLight = false;
  try {
    prefersLight = window.matchMedia('(prefers-color-scheme: light)').matches;
  } catch (e) { /* no matchMedia */ }
  var theme = (saved === 'light' || saved === 'dark')
    ? saved
    : (prefersLight ? 'light' : 'dark');
  document.documentElement.setAttribute('data-theme', theme);
})();
