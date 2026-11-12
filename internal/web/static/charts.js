// Opens a chart's overlay.
//
// Each chart card is a link to /charts?attr=..., which shows the same large
// chart and table on a page of its own; that is what a browser without
// script gets. With script, the link opens the <dialog> already in the page
// instead. A modal dialog closes on Esc by itself and returns focus to the
// card that opened it.
(function () {
  document.addEventListener("click", function (e) {
    var open = e.target.closest("[data-dialog]");
    if (open && !e.ctrlKey && !e.metaKey && !e.shiftKey && e.button === 0) {
      var d = document.getElementById(open.getAttribute("data-dialog"));
      if (d && typeof d.showModal === "function") {
        e.preventDefault();
        d.showModal();
      }
      return;
    }
    if (e.target.closest("[data-close]")) {
      var dlg = e.target.closest("dialog");
      if (dlg) dlg.close();
      return;
    }
    // A click on the backdrop lands on the dialog element itself; the
    // content sits in an inner wrapper.
    if (e.target.tagName === "DIALOG" && e.target.open) {
      e.target.close();
    }
  });

  // Following a link out of an overlay swaps the page under it; close it
  // first so the top layer does not outlive its content.
  document.addEventListener("htmx:beforeRequest", function () {
    document.querySelectorAll("dialog[open]").forEach(function (d) { d.close(); });
  });
})();
