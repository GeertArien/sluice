// Click-to-copy SHAs (spec §6).
document.addEventListener("click", (e) => {
  const el = e.target.closest(".sha");
  if (!el) return;
  navigator.clipboard.writeText(el.dataset.sha || el.textContent).then(() => {
    el.classList.add("copied");
    setTimeout(() => el.classList.remove("copied"), 800);
  });
});

// Copy buttons: copy the value of their data-copy attribute (e.g. SSH key).
document.addEventListener("click", (e) => {
  const btn = e.target.closest("[data-copy]");
  if (!btn) return;
  navigator.clipboard.writeText(btn.getAttribute("data-copy")).then(() => {
    const old = btn.textContent;
    btn.textContent = "copied!";
    setTimeout(() => { btn.textContent = old; }, 1000);
  });
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
