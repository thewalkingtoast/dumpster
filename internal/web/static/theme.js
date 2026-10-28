// Applies the saved (or system) theme before the first paint.
//
// This is a separate file, loaded synchronously in <head>, for two reasons: the
// CSP forbids inline scripts, and a deferred script would run after the browser
// has already painted the default palette — which is exactly the flash of wrong
// colours this exists to prevent.
(function () {
  var KEY = 'dumpster-theme';
  var root = document.documentElement;
  var saved = null;
  try { saved = window.localStorage.getItem(KEY); } catch (e) { /* private mode */ }
  var prefersLight = false;
  try {
    prefersLight = window.matchMedia('(prefers-color-scheme: light)').matches;
  } catch (e) { /* no matchMedia */ }
  var theme = (saved === 'light' || saved === 'dark')
    ? saved
    : (prefersLight ? 'light' : 'dark');
  root.setAttribute('data-theme', theme);

  function current() {
    return root.getAttribute('data-theme') === 'light' ? 'light' : 'dark';
  }

  // Repaint every switch on the page: the label describes the mode a click will
  // move to, so it has to flip with the theme.
  function paint() {
    var next = current() === 'light' ? 'dark' : 'light';
    var label = 'Switch to ' + next + ' mode';
    var buttons = document.querySelectorAll('[data-theme-toggle]');
    for (var i = 0; i < buttons.length; i++) {
      buttons[i].setAttribute('aria-label', label);
      buttons[i].setAttribute('title', label);
      buttons[i].setAttribute('aria-pressed', current() === 'light' ? 'true' : 'false');
    }
  }

  function flip() {
    var next = current() === 'light' ? 'dark' : 'light';
    root.setAttribute('data-theme', next);
    try { window.localStorage.setItem(KEY, next); } catch (e) { /* private mode */ }
    paint();
  }

  function wire() {
    paint();
    // Delegated, so the landing page, the error page and a dumpster page all get
    // the same behaviour without their own script.
    document.addEventListener('click', function (ev) {
      var btn = ev.target && ev.target.closest && ev.target.closest('[data-theme-toggle]');
      if (btn) flip();
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', wire);
  } else {
    wire();
  }
})();
