// SPDX-License-Identifier: Apache-2.0
// Loaded before the page is drawn (not deferred), so the theme never flashes. The choice is
// "light", "dark", or none (follow the system), kept in localStorage.
(function () {
  var choice = null;
  try { choice = localStorage.getItem("theme"); } catch (e) {}
  var dark = choice === "dark" || (choice !== "light" && window.matchMedia("(prefers-color-scheme: dark)").matches);
  document.documentElement.classList.toggle("dark", dark);
})();
