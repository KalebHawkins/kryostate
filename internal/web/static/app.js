// Shows the body of a refused request instead of discarding it.
//
// htmx ignores the response body on a 4xx by default. The server answers a
// second collection request with 409 and a sentence explaining why, and
// without this the button would appear to do nothing at all.
document.addEventListener("htmx:beforeSwap", function (e) {
  if (e.detail.xhr.status >= 400 && e.detail.xhr.status < 500) {
    e.detail.shouldSwap = true;
    e.detail.isError = false;
  }
});

// Filters the collectors list as the person types. The same form submits to
// the server without script, which applies the same rules; this only saves
// the round trip, and keeps the URL in step so a filtered list can be shared.
(function () {
  function apply(form) {
    var list = document.querySelector(form.getAttribute("data-filter"));
    if (!list) return;
    var q = (form.elements.q.value || "").toLowerCase().split(/\s+/).filter(Boolean);
    var cat = form.elements.cat.value, source = form.elements.source.value, status = form.elements.status.value;
    var shown = 0, total = 0;
    list.querySelectorAll("tbody[data-section]").forEach(function (section) {
      var any = false;
      section.querySelectorAll("tr[data-row]").forEach(function (row) {
        var text = row.getAttribute("data-text") || "";
        var ok = (!cat || row.getAttribute("data-cat") === cat) &&
          (!source || row.getAttribute("data-source") === source) &&
          (!status || row.getAttribute("data-status") === status) &&
          q.every(function (w) { return text.indexOf(w) >= 0; });
        row.hidden = !ok;
        total++;
        if (ok) { shown++; any = true; }
      });
      section.hidden = !any;
    });
    var count = form.querySelector("[data-filter-count]");
    if (count) count.textContent = shown + " of " + total;
    var empty = document.querySelector("[data-filter-empty]");
    if (empty) empty.hidden = shown > 0;
    var params = new URLSearchParams();
    if (form.elements.q.value) params.set("q", form.elements.q.value);
    if (cat) params.set("cat", cat);
    if (source) params.set("source", source);
    if (status) params.set("status", status);
    var qs = params.toString();
    history.replaceState(history.state, "", location.pathname + (qs ? "?" + qs : ""));
  }
  function filterForm(el) { return el && el.closest ? el.closest("form[data-filter]") : null; }
  document.addEventListener("input", function (e) { var f = filterForm(e.target); if (f) apply(f); });
  document.addEventListener("change", function (e) { var f = filterForm(e.target); if (f) apply(f); });
  document.addEventListener("submit", function (e) {
    var f = filterForm(e.target);
    if (f) { e.preventDefault(); apply(f); }
  });
  // With script the list filters itself, so the button is not needed.
  function hideButtons(root) {
    (root || document).querySelectorAll("[data-filter-submit]").forEach(function (b) { b.hidden = true; });
  }
  document.addEventListener("DOMContentLoaded", function () { hideButtons(); });
  document.addEventListener("htmx:load", function (e) { hideButtons(e.target); });
})();

// Copies a definition to the clipboard. The clipboard API needs a secure
// context, which a server reached over plain HTTP on the network is not, so
// selecting the text and copying it is the fallback.
document.addEventListener("click", function (e) {
  var btn = e.target.closest ? e.target.closest("[data-copy]") : null;
  if (!btn) return;
  var src = document.querySelector(btn.getAttribute("data-copy"));
  if (!src) return;
  var label = btn.getAttribute("data-label") || btn.textContent;
  btn.setAttribute("data-label", label);
  function done(ok) {
    btn.textContent = ok ? "Copied" : "Press Ctrl+C to copy";
    setTimeout(function () { btn.textContent = label; }, 1500);
  }
  if (navigator.clipboard && window.isSecureContext) {
    navigator.clipboard.writeText(src.textContent).then(function () { done(true); }, function () { done(false); });
    return;
  }
  var range = document.createRange();
  range.selectNodeContents(src);
  var sel = window.getSelection();
  sel.removeAllRanges();
  sel.addRange(range);
  var ok = false;
  try { ok = document.execCommand("copy"); } catch (err) { ok = false; }
  done(ok);
});
