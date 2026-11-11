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
