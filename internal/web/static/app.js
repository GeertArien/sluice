// Copy helper. navigator.clipboard only exists in a secure context
// (HTTPS or localhost); Sluice is often served over plain http://<lan-ip>,
// so fall back to a temporary textarea + execCommand("copy").
function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) {
    return navigator.clipboard.writeText(text);
  }
  return new Promise((resolve, reject) => {
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.setAttribute("readonly", "");
    ta.style.position = "fixed";
    ta.style.top = "-9999px";
    document.body.appendChild(ta);
    ta.select();
    let ok = false;
    try {
      ok = document.execCommand("copy");
    } catch (_) {
      ok = false;
    }
    document.body.removeChild(ta);
    ok ? resolve() : reject(new Error("copy failed"));
  });
}

function flash(el, text) {
  const old = el.textContent;
  el.textContent = text;
  setTimeout(() => { el.textContent = old; }, 1000);
}

// Click-to-copy SHAs (spec §6).
document.addEventListener("click", (e) => {
  const el = e.target.closest(".sha");
  if (!el) return;
  copyText(el.dataset.sha || el.textContent).then(() => {
    el.classList.add("copied");
    setTimeout(() => el.classList.remove("copied"), 800);
  }).catch(() => {});
});

// Copy buttons: copy the value of their data-copy attribute (e.g. SSH key).
document.addEventListener("click", (e) => {
  const btn = e.target.closest("[data-copy]");
  if (!btn) return;
  copyText(btn.getAttribute("data-copy"))
    .then(() => flash(btn, "copied!"))
    .catch(() => flash(btn, "copy failed"));
});

// Confirm dialogs restating exactly what will happen (spec §6).
document.addEventListener("submit", (e) => {
  const form = e.target.closest("form[data-confirm]");
  if (form && !window.confirm(form.dataset.confirm)) e.preventDefault();
});

// Live job log polling (spec §5.6).
const logEl = document.getElementById("joblog");
if (logEl && logEl.dataset.poll === "1") {
  const id = logEl.dataset.jobId;
  const tick = async () => {
    try {
      const resp = await fetch(`/jobs/${id}/log`, { cache: "no-store" });
      const text = await resp.text();
      if (text !== logEl.textContent) {
        logEl.textContent = text;
        logEl.scrollTop = logEl.scrollHeight;
      }
      const status = resp.headers.get("X-Job-Status");
      if (status === "queued" || status === "running") {
        setTimeout(tick, 2000);
      } else {
        window.location.reload(); // pick up final status, duration, retry button
      }
    } catch {
      setTimeout(tick, 5000);
    }
  };
  setTimeout(tick, 2000);
}
