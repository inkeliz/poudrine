"use strict";

function el(id) {
  return document.getElementById(id);
}

// HTML carries the formatting locale negotiated from Accept-Language.
// The same script also runs as a service worker, without a document.
const display = typeof document === "undefined" ? null : pageFormats(document.documentElement.dataset.locale || "en");

function pageFormats(locale) {
  const number = new Intl.NumberFormat(locale).format;
  const percent = new Intl.NumberFormat(locale, {style: "percent", maximumFractionDigits: 1}).format;
  const relative = new Intl.RelativeTimeFormat(locale, {numeric: "auto"});
  const list = new Intl.ListFormat(locale, {type: "unit", style: "short"});
  const collate = new Intl.Collator(locale, {numeric: true});
  const units = new Map();
  function unit(value, name, width = "short") {
    const key = name + "/" + width;
    if (!units.has(key)) {
      units.set(key, new Intl.NumberFormat(locale, {style: "unit", unit: name, unitDisplay: width, maximumFractionDigits: 2}));
    }
    return units.get(key).format(value);
  }
  return {locale, number, percent, relative, list, collate, unit};
}

function serverTime(stamp, options) {
  const zone = stamp?.match(/(Z|[+-]\d{2}:\d{2})$/)?.[1];
  const instant = new Date(stamp);
  if (!zone || Number.isNaN(instant.getTime())) return "";
  const timeZone = zone === "Z" ? "UTC" : zone;
  try {
    return new Intl.DateTimeFormat(display.locale, {...options, timeZone}).format(instant);
  } catch (error) {
    if (!(error instanceof RangeError)) throw error;
    // Older browsers only accept named time zones. Format the server's wall
    // clock in UTC, then restore its offset in the locale's time-zone field.
    const wall = new Date(stamp.replace(/(Z|[+-]\d{2}:\d{2})$/, "Z"));
    const parts = new Intl.DateTimeFormat(display.locale, {...options, timeZone: "UTC"}).formatToParts(wall);
    return parts.map(p => p.type === "timeZoneName" ? "UTC" + zone : p.value).join("");
  }
}

if (typeof document === "undefined") {
  listenForPackageNotifications();
} else if (document.body.classList.contains("client-page")) {
  initClientSetup();
} else {
  initManagement();
}

function listenForPackageNotifications() {
  // Existing subscriptions use this worker immediately after an update.
  self.addEventListener("install", event => event.waitUntil(self.skipWaiting()));
  self.addEventListener("push", event => {
    const options = {body: event.data?.text() || "A newer package build is available."};
    try {
      const message = JSON.parse(options.body);
      if (typeof message?.body === "string") {
        options.body = message.body;
        if (typeof message.tag === "string") options.tag = message.tag;
      }
    } catch {
      // Push services may still have plain-text messages from an older installation.
    }
    event.waitUntil(self.registration.showNotification("Poudrine", options));
  });
  self.addEventListener("notificationclick", event => {
    event.notification.close();
    event.waitUntil(self.clients.openWindow(new URL("admin", self.registration.scope).href));
  });
}

function initClientSetup() {
  const feedback = el("copy-feedback");
  let feedbackTimer;

  function announce(text) {
    clearTimeout(feedbackTimer);
    feedback.textContent = text;
    feedbackTimer = setTimeout(() => { feedback.textContent = ""; }, 4500);
  }

  async function copySource(button) {
    const source = el(button.dataset.copy);
    try {
      await navigator.clipboard.writeText(source.textContent);
    } catch {
      // Select the rendered snippet on HTTP or when clipboard access is denied.
      const details = source.closest("details");
      if (details) details.open = true;
      source.closest("pre")?.focus();
      const range = document.createRange();
      range.selectNodeContents(source);
      const selection = window.getSelection();
      selection.removeAllRanges();
      selection.addRange(range);
      let copied = false;
      try {
        copied = document.execCommand("copy");
      } catch {
        // The selected text is still available to the browser's Copy command.
      }
      if (!copied) {
        announce("Text selected. Use your device’s Copy command.");
        return;
      }
    }
    announce("Copied to clipboard.");
  }

  for (const button of document.querySelectorAll("[data-copy]")) {
    button.hidden = false;
    button.addEventListener("click", () => copySource(button));
  }

  function showUpdated(stamp) {
    const element = el("catalog-updated");
    if (!stamp) {
      element.removeAttribute("datetime");
      element.textContent = "Pending";
      return;
    }
    const time = new Date(stamp);
    element.dateTime = time.toISOString();
    element.textContent = time.toLocaleString(display.locale, {dateStyle: "medium", timeStyle: "short"});
  }

  function showCatalog(page) {
    const stamp = page.getElementById("catalog-updated").getAttribute("datetime");
    for (const id of ["catalog-packages", "catalog-ahead"]) {
      el(id).textContent = page.getElementById(id).textContent;
    }
    const state = el("catalog-state");
    const ready = stamp ? "yes" : "no";
    if (state.dataset.ready !== ready) {
      state.dataset.ready = ready;
      el("catalog-state-label").textContent = stamp ? "Catalog available" : "Waiting for catalog";
    }
    showUpdated(stamp);
  }

  // The initial HTML already contains the complete setup and catalog snapshot.
  showCatalog(document);
  let checking = false;
  let lastCheck = Date.now();
  async function refreshCatalog() {
    if (checking || document.hidden) return;
    checking = true;
    lastCheck = Date.now();
    el("catalog-retry").disabled = true;
    try {
      const response = await fetch("./", {headers: {Accept: "text/html"}, cache: "no-store", redirect: "error"});
      if (!response.ok) throw new Error("Catalog unavailable");
      showCatalog(new DOMParser().parseFromString(await response.text(), "text/html"));
      el("catalog-error").hidden = true;
    } catch {
      el("catalog-error").hidden = false;
    } finally {
      el("catalog-retry").disabled = false;
      checking = false;
    }
  }

  el("catalog-retry").addEventListener("click", refreshCatalog);
  const refreshIfDue = () => { if (Date.now() - lastCheck >= 60000) refreshCatalog(); };
  document.addEventListener("visibilitychange", refreshIfDue);
  setInterval(refreshIfDue, 60000);
}

function buildDuration(seconds, width = "narrow") {
  if (!Number.isFinite(seconds) || seconds < 0) return "—";
  seconds = Math.floor(seconds);
  const hours = Math.floor(seconds / 3600), minutes = Math.floor(seconds % 3600 / 60);
  const parts = [];
  if (hours) parts.push(display.unit(hours, "hour", width));
  if (minutes) parts.push(display.unit(minutes, "minute", width));
  if ((!hours || width === "long") && (seconds % 60 || !parts.length)) {
    parts.push(display.unit(seconds % 60, "second", width));
  }
  return display.list.format(parts);
}

function relativeTime(seconds) {
  for (const [scale, unit] of [[86400, "day"], [3600, "hour"], [60, "minute"]]) {
    if (Math.abs(seconds) >= scale) return display.relative.format(Math.trunc(seconds / scale), unit);
  }
  return display.relative.format(Math.trunc(seconds), "second");
}

async function readBuildEvents(response, onBuild) {
  const type = response.headers.get("Content-Type")?.split(";")[0].trim().toLowerCase();
  if (type !== "text/event-stream" || !response.body) throw new Error("Expected a build event stream.");
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  try {
    while (true) {
      const {done, value} = await reader.read();
      if (done) return;
      buffer += decoder.decode(value, {stream: true});
      const frames = buffer.split(/\r?\n\r?\n/);
      buffer = frames.pop();
      for (const frame of frames) {
        let event;
        const data = [];
        for (const line of frame.split(/\r?\n/)) {
          if (line.startsWith("event:")) event = line.slice(6).trimStart();
          else if (line.startsWith("data:")) data.push(line.slice(5).replace(/^ /, ""));
        }
        if (event === "build" && data.length) onBuild(JSON.parse(data.join("\n")));
      }
    }
  } finally { reader.releaseLock(); }
}

function focusRunningLog(button) {
  const list = el("build-jobs");
  const current = list.querySelector('[data-log-job="' + CSS.escape(button.dataset.logJob) + '"][data-log-run="' + CSS.escape(button.dataset.logRun) + '"]');
  const slot = list.querySelector('[data-builder="' + CSS.escape(button.dataset.builder) + '"]');
  (current || slot || el("refresh")).focus({preventScroll: true});
}

function createBuildResults(request, el, node) {
  // Worker timestamps can have a UTC offset. Keep nanoseconds when matching
  // them to Poudriere's UTC run names, even though Date displays milliseconds.
  function runInstant(started) {
    if (!started || started.startsWith("0001-01-01")) return "";
    const seconds = new Date(started).toISOString().split(".")[0];
    const fraction = started.match(/\.(\d+)/)?.[1] || "";
    return seconds + "." + fraction.padEnd(9, "0");
  }

  const panel = el("build-results"), selector = el("result-run"), list = el("result-list");
  const older = el("result-older"), newer = el("result-newer"), hideEmpty = el("result-hide-empty");
  const filters = el("result-filter").querySelectorAll("[data-outcome]"), groupRetries = [];
  const status = el("result-status"), feedback = el("result-feedback"), more = el("result-more");
  const dialog = el("package-log-dialog"), output = el("package-log-output"), earlier = el("package-log-earlier");
  const runDate = new Intl.DateTimeFormat(display.locale, {dateStyle: "medium", timeStyle: "long"});
  let view, selected = "", listVersion = 0, focusPackage, worker, submitting = false, retryVersion = 0;
  let entries = [], visible = [], shown = 0, selectedOutcome = "all";
  const outcomes = {succeeded: "Built", failed: "Failed", skipped: "Skipped"};
  const pageSize = 50;
  const latest = el("package-log-latest");
  let logArgs, logStart, logTrigger, logVersion = 0;

  function retryControls() {
    const disabled = submitting || !worker?.can_force;
    for (const button of groupRetries) {
      button.disabled = disabled || !view?.started || !view[button.dataset.retryOutcome]?.length;
    }
    for (const button of list.querySelectorAll("[data-retry]")) button.disabled = disabled;
  }

  async function retry(args) {
    if (submitting || !worker?.can_force) return;
    submitting = true;
    const version = retryVersion;
    const started = args.started;
    retryControls();
    const target = args.package || args.outcome + " packages";
    feedback.textContent = "Queuing " + target + "…";
    try {
      const reply = await request("/api/admin/build/retry", "POST", args);
      if (!reply.queued) throw new Error("The worker did not accept the retry.");
      if (version !== retryVersion || view?.started !== started || !panel.open) return;
      feedback.textContent = reply.already_queued ? "The pending build includes " + target + "." : "Retry queued for " + target + ".";
      if (worker?.window_opens) feedback.textContent += " The worker will resume during working hours.";
    } catch (error) {
      if (version === retryVersion && view?.started === started && panel.open) feedback.textContent = "Could not confirm the retry. " + error.message;
    } finally {
      submitting = false;
      retryControls();
    }
  }

  function retryButton(label, onClick) {
    const button = el("package-retry-action").content.firstElementChild.cloneNode(true);
    button.title = label;
    button.setAttribute("aria-label", label);
    button.onclick = onClick;
    button.disabled = submitting || !worker?.can_force;
    return button;
  }

  for (const segment of el("result-filter").querySelectorAll(".result-segment")) {
    const outcome = segment.firstElementChild.dataset.outcome;
    const button = retryButton("Retry all " + outcome + " packages in this run", () => {
      if (view?.started) return retry({started: view.started, outcome});
    });
    button.dataset.retryOutcome = outcome;
    groupRetries.push(button);
    segment.append(button);
  }

  async function loadLog(fromEnd = false) {
    if (!logArgs || latest.disabled) return;
    const version = ++logVersion;
    const before = fromEnd ? undefined : logStart;
    latest.disabled = earlier.disabled = true;
    el("package-log-status").textContent = "Loading package output…";
    try {
      const args = before == null ? logArgs : {...logArgs, before};
      const page = await request("/api/admin/build/log", "QUERY", args);
      if (version !== logVersion || !dialog.open) return;
      const height = output.scrollHeight, top = output.scrollTop;
      if (before == null) output.textContent = page.text;
      else output.prepend(document.createTextNode(page.text));
      logStart = page.start;
      earlier.hidden = page.start === 0;
      earlier.textContent = "Load earlier output";
      el("package-log-status").textContent = page.size === 0 ? "The log is empty." :
        page.start > 0 ? "Showing output from byte " + display.number(page.start) + " of " + display.number(page.size) + "." : "Showing the log from the beginning.";
      output.scrollTop = before == null ? output.scrollHeight : top + output.scrollHeight - height;
    } catch (error) {
      if (version !== logVersion || !dialog.open) return;
      el("package-log-status").textContent = "Could not read this package’s log. " + error.message;
    } finally {
      if (version === logVersion) latest.disabled = earlier.disabled = false;
    }
  }

  function showLog(started, pkg, phase, trigger) {
    logTrigger = trigger;
    logArgs = {started, package: pkg.package};
    logStart = undefined;
    output.replaceChildren();
    earlier.hidden = true;
    latest.disabled = earlier.disabled = false;
    el("package-log-title").textContent = pkg.package;
    el("package-log-info").textContent = [pkg.origin, phase, runDate.format(new Date(started))].filter(Boolean).join(" · ");
    dialog.showModal();
    loadLog();
  }
  earlier.onclick = () => loadLog();
  latest.onclick = () => loadLog(true);
  el("close-package-log").onclick = () => dialog.close();
  dialog.addEventListener("close", () => {
    if (dialog.open) return;
    ++logVersion;
    // Live job updates can replace the button while its dialog is open.
    if (logTrigger?.dataset.logJob && !logTrigger.isConnected) focusRunningLog(logTrigger);
    logArgs = logTrigger = undefined;
    output.replaceChildren();
  });

  function logButton(started, pkg, phase, button = el("package-log-action").content.firstElementChild.cloneNode(true)) {
    button.title = "View build log for " + pkg.package;
    button.setAttribute("aria-label", button.title);
    button.onclick = () => showLog(started, pkg, phase, button);
    return button;
  }

  function dependencyList(summary, packages, describe) {
    const details = document.createElement("details"), list = document.createElement("ul");
    details.className = "result-deps";
    details.append(node("summary", summary), list);
    details.addEventListener("toggle", () => {
      if (details.open && !list.childElementCount) {
        const rows = document.createDocumentFragment();
        for (const pkg of packages) rows.append(node("li", describe(pkg)));
        list.append(rows);
      }
    });
    return details;
  }

  function resultRow({pkg, outcome, children, blockers}) {
    const started = view.started;
    const item = document.createElement("li");
    item.className = "result-row";
    item.tabIndex = -1;
    item.dataset.package = pkg.package;
    const state = document.createElement("div"), name = document.createElement("div");
    state.className = "result-state";
    const label = node("span", outcomes[outcome]);
    label.className = "result-outcome";
    label.dataset.outcome = outcome;
    state.append(label);
    if (Number.isFinite(pkg.build_seconds)) {
      const time = node("small", buildDuration(pkg.build_seconds));
      time.title = "Build time: " + buildDuration(pkg.build_seconds, "long");
      state.append(time);
    }
    name.className = "result-name";
    name.append(node("strong", pkg.origin), node("small", pkg.package));
    item.append(state, name);
    const actions = document.createElement("div");
    actions.className = "result-actions";
    if (outcome !== "succeeded") {
      const detail = document.createElement("div");
      detail.className = "result-detail";
      if (outcome === "failed") {
        const reason = [pkg.phase && "Phase " + pkg.phase, pkg.reason].filter(Boolean).join(" · ");
        if (reason) detail.append(node("p", reason));
        if (children?.length) detail.append(dependencyList("Skipped dependents (" + display.number(children.length) + ")",
          children, child => child.origin + " · " + child.package));
      } else if (blockers.length === 1) {
        detail.append(node("p", "Blocked by " + blockers[0]));
      } else if (blockers.length > 1) {
        detail.append(dependencyList("Blocked by " + display.number(blockers.length) + " packages", blockers, name => name));
      }
      if (detail.childElementCount) item.append(detail);
      actions.append(retryButton("Retry " + pkg.package + (children?.length ? " with dependents" : ""),
        () => retry({started, package: pkg.package})));
    }
    if (outcome !== "skipped") actions.append(logButton(started, pkg, [outcomes[outcome], pkg.phase].filter(Boolean).join(" · ")));
    item.append(actions);
    return item;
  }

  function runOption(started, count, latest = false) {
    const label = started ? [latest ? "Latest" : "", runDate.format(new Date(started)),
      Number.isFinite(count) ? display.number(count) + " compiled" : "Count unavailable"].filter(Boolean).join(" · ") : "No runs";
    const option = node("option", label);
    option.value = started;
    return option;
  }

  function updateNavigation() {
    const index = selector.selectedIndex;
    newer.disabled = index <= 0;
    older.disabled = index < 0 || index >= selector.options.length - 1;
  }

  function showMore(count = pageSize) {
    const rows = document.createDocumentFragment();
    for (const entry of visible.slice(shown, shown + count)) rows.append(resultRow(entry));
    shown += rows.childElementCount;
    list.append(rows);
    more.hidden = shown >= visible.length;
  }

  function renderPackages(limit = pageSize) {
    visible = selectedOutcome === "all" ? entries : entries.filter(entry => entry.outcome === selectedOutcome);
    list.replaceChildren();
    shown = 0;
    if (focusPackage) {
      const index = visible.findIndex(entry => entry.pkg.package === focusPackage);
      limit = Math.max(limit, index + 1);
    }
    showMore(limit);
    if (!visible.length && entries.length) list.append(node("li", "No packages with this result in this run."));
  }
  more.onclick = () => showMore();

  function selectOutcome(outcome) {
    selectedOutcome = outcome;
    for (const button of filters) button.setAttribute("aria-pressed", button.dataset.outcome === outcome);
  }
  for (const button of filters) {
    button.onclick = () => {
      selectOutcome(button.dataset.outcome);
      focusPackage = undefined;
      if (view) renderPackages();
      refresh();
    };
  }

  function showCounts(counts) {
    for (const button of filters) {
      const count = button.firstElementChild;
      if (count) count.textContent = counts ? display.number(counts[button.dataset.outcome]) : "—";
    }
  }

  function render(data) {
    const runs = data.runs || [];
    const options = document.createDocumentFragment();
    for (const start of runs) options.append(runOption(start, data.compiled?.[start], start === runs[0]));
    if (!runs.length) options.append(runOption(""));
    selector.replaceChildren(options);
    selector.value = data.started || "";
    selector.disabled = !runs.length;
    updateNavigation();

    const started = data.started;
    const completed = data.succeeded || [], failures = data.failed || [];
    const skipped = new Map(), blocked = new Map();
    for (const pkg of data.skipped || []) {
      let entry = skipped.get(pkg.package);
      if (!entry) {
        entry = {outcome: "skipped", pkg, blockers: []};
        skipped.set(pkg.package, entry);
      }
      if (pkg.blocked_by && !entry.blockers.includes(pkg.blocked_by)) {
        entry.blockers.push(pkg.blocked_by);
        if (!blocked.has(pkg.blocked_by)) blocked.set(pkg.blocked_by, []);
        blocked.get(pkg.blocked_by).push(pkg);
      }
    }
    entries = [
      ...completed.map(pkg => ({pkg, outcome: "succeeded"})),
      ...failures.map(pkg => ({pkg, outcome: "failed", children: blocked.get(pkg.package)})),
      ...skipped.values()
    ];
    entries.sort((a, b) => display.collate.compare(a.pkg.package, b.pkg.package));
    renderPackages(Math.max(shown, pageSize));
    showCounts(started && {succeeded: completed.length, failed: failures.length, skipped: skipped.size});
    retryControls();
    if (!started) {
      status.textContent = hideEmpty.checked ? "No runs have package results. Choose All runs to include empty runs." : "No build runs are available yet.";
      return;
    }
    const active = worker?.active && runInstant(worker.started) === runInstant(started);
    status.textContent = entries.length ? "" : active ? "Waiting for package results." : "No package results were recorded.";
  }

  async function refresh() {
    if (!panel.open) return;
    const version = ++listVersion;
    if (!view) status.textContent = "Loading build results…";
    try {
      const args = selected ? {started: selected} : {};
      if (hideEmpty.checked) args.hide_empty = true;
      let data = await request("/api/admin/build/results", "QUERY", args);
      if (version !== listVersion || !panel.open) return;
      // A new run may have no Poudriere log directory yet. Link the
      // older retained runs in the selector while the latest one is empty.
      if (!hideEmpty.checked && runInstant(worker?.started) > runInstant(data.runs?.[0])) {
        if (!selected) data = {started: worker.started, runs: data.runs, compiled: data.compiled};
        data.runs = [worker.started, ...(data.runs || [])];
        data.compiled = {...data.compiled, [worker.started]: 0};
      }
      if (selected) selected = data.started || selected;
      if (view?.started !== data.started) {
        shown = 0;
        feedback.textContent = "";
      }
      view = data;
      render(view);
      if (focusPackage) {
        const row = Array.from(list.children).find(item => item.dataset.package === focusPackage);
        (row || selector).focus();
        if (!row) feedback.textContent = "Details for " + focusPackage + " are unavailable in this run.";
        focusPackage = undefined;
      }
    } catch (error) {
      if (version !== listVersion || !panel.open) return;
      clearResults();
      status.textContent = "Could not load build results. " + error.message;
    }
  }

  function clearResults() {
    ++retryVersion;
    view = undefined;
    entries = visible = [];
    shown = 0;
    list.replaceChildren();
    more.hidden = true;
    showCounts();
    retryControls();
    status.textContent = feedback.textContent = "";
  }

  function chooseRun(started) {
    selected = started;
    clearResults();
    if (selected && !Array.from(selector.options).some(option => option.value === selected)) {
      selector.add(runOption(started));
    }
    selector.value = selected || selector.options[0]?.value || "";
    updateNavigation();
    if (!panel.open) panel.open = true;
    else refresh();
  }
  selector.onchange = () => {
    focusPackage = undefined;
    selectOutcome("all");
    chooseRun(selector.selectedIndex === 0 ? "" : selector.value);
  };
  function stepRun(offset) {
    selector.selectedIndex += offset;
    selector.onchange();
  }
  older.onclick = () => stepRun(1);
  newer.onclick = () => stepRun(-1);
  for (const radio of [hideEmpty, el("result-all-runs")]) {
    radio.onchange = () => {
      focusPackage = undefined;
      chooseRun(hideEmpty.checked && !entries.length ? "" : selected);
    };
  }
  panel.addEventListener("toggle", () => { if (panel.open) refresh(); });

  return {
    logButton,
    reveal(started, pkg) {
      focusPackage = pkg;
      selectOutcome("failed");
      chooseRun(started);
    },
    update(build) {
      const prior = worker;
      worker = build;
      if (prior?.can_force !== build.can_force) retryControls();
      const started = runInstant(build.started);
      const changedRun = runInstant(prior?.started) !== started;
      if (changedRun && !selected) clearResults();
      if (!panel.open) return;
      const watchingWorker = !selected || runInstant(selected) === started;
      if (changedRun || prior?.finished !== build.finished || watchingWorker &&
          (prior?.stage !== build.stage || ["built", "failed", "skipped"].some(key => prior?.progress?.[key] !== build.progress?.[key]))) {
        refresh();
      }
    },
    close() {
      ++listVersion;
      if (dialog.open) dialog.close();
      panel.open = false;
      focusPackage = worker = undefined;
      selected = "";
      selectOutcome("all");
      el("result-all-runs").checked = true;
      clearResults();
      selector.replaceChildren(runOption(""));
      selector.disabled = true;
      updateNavigation();
    }
  };
}

function createHistoryPeriods(request, el, node) {
  const plot = el("history-plot"), chart = el("chart-bars"), panel = el("history-period");
  const list = el("history-period-results"), status = el("history-period-status"), more = el("history-period-more");
  const choices = new WeakMap();
  let selected;

  async function load(view) {
    if (view.loading) return;
    view.loading = true;
    status.textContent = "Loading packages…";
    more.setAttribute("aria-disabled", "true");
    try {
      const page = await request("/api/admin/build/history/packages", "QUERY", {...view.query, after: view.next});
      if (selected !== view) return;
      const rows = document.createDocumentFragment();
      for (const result of page.packages) {
        const row = node("li"), pkg = node("span", result.package);
        const outcome = node("span", result.outcome === "failed" ? "Failed" : "Built");
        pkg.className = "history-package";
        outcome.className = "history-outcome";
        outcome.dataset.outcome = result.outcome;
        row.append(pkg, outcome);
        rows.append(row);
      }
      const firstNew = rows.firstElementChild;
      list.append(rows);
      if (document.activeElement === more) {
        // Continue reading at the first newly revealed package, even when
        // this was the last page and the pagination button disappears.
        if (firstNew) {
          firstNew.tabIndex = -1;
          firstNew.focus();
        } else el("history-period-close").focus({preventScroll: true});
      }
      view.next = page.next;
      status.textContent = page.next ? "Showing the first " + display.number(list.childElementCount) + " results." :
        list.childElementCount ? "" : "No packages finished in this period.";
      more.textContent = "Show more packages";
      more.hidden = !page.next;
    } catch (error) {
      if (selected !== view) return;
      status.textContent = "Could not load package results. " + error.message;
      more.textContent = "Retry";
      more.hidden = false;
    } finally {
      view.loading = false;
      if (selected === view) more.removeAttribute("aria-disabled");
    }
  }

  function open(bar) {
    selected?.bar.setAttribute("aria-expanded", "false");
    const choice = choices.get(bar);
    selected = {...choice, bar};
    bar.setAttribute("aria-expanded", "true");
    panel.hidden = false;
    el("history-period-title").textContent = choice.label;
    el("history-period-summary").textContent = choice.summary;
    list.replaceChildren();
    list.scrollTop = 0;
    more.hidden = true;
    if (!choice.total) status.textContent = "No packages finished in this period.";
    else load(selected);
  }

  function close(restoreFocus = false) {
    if (restoreFocus) selected?.bar.focus({preventScroll: true});
    selected?.bar.setAttribute("aria-expanded", "false");
    selected = undefined;
    panel.hidden = true;
    list.replaceChildren();
  }

  function tabStop(bar) {
    chart.querySelector('[tabindex="0"]')?.setAttribute("tabindex", "-1");
    bar.setAttribute("tabindex", "0");
  }

  function activate(bar) {
    tabStop(bar);
    if (selected?.bar === bar) close();
    else open(bar);
  }

  chart.addEventListener("focusin", event => {
    if (choices.has(event.target)) tabStop(event.target);
  });
  chart.onclick = event => {
    const bar = event.target.closest("g[data-start]");
    if (bar) activate(bar);
  };
  chart.onkeydown = event => {
    const bar = event.target;
    if (!choices.has(bar)) return;
    switch (event.key) {
      case "ArrowLeft": (bar.previousElementSibling || bar).focus(); break;
      case "ArrowRight": (bar.nextElementSibling || bar).focus(); break;
      case "Home": chart.firstElementChild.focus(); break;
      case "End": chart.lastElementChild.focus(); break;
      case " ":
      case "Enter": if (!event.repeat) activate(bar); break;
      default: return;
    }
    event.preventDefault();
  };
  plot.onkeydown = event => {
    if (event.key === "Escape" && selected) { close(true); event.preventDefault(); }
  };
  el("history-period-close").onclick = () => close(true);
  more.onclick = () => { if (selected) load(selected); };

  return {
    close,
    register(bar, bucket, interval, asOf, label) {
      const summary = display.number(bucket.succeeded) + " built · " + display.number(bucket.failed) + " failed";
      choices.set(bar, {query: {interval, start: bucket.start, as_of: asOf}, label, summary, total: bucket.succeeded + bucket.failed});
      bar.dataset.start = bucket.start;
      bar.setAttribute("role", "button");
      bar.setAttribute("tabindex", "-1");
      bar.setAttribute("aria-label", label + ", " + summary);
      bar.setAttribute("aria-controls", panel.id);
      bar.setAttribute("aria-expanded", "false");
    },
    replace(bars) {
      const columns = Array.from(bars.children);
      const oldTab = chart.querySelector('[tabindex="0"]');
      const tab = columns.find(col => col.dataset.start === oldTab?.dataset.start) || columns.at(-1);
      tab?.setAttribute("tabindex", "0");
      const focused = chart.contains(document.activeElement);
      const next = selected && columns.find(col => col.dataset.start === selected.query.start);
      chart.replaceChildren(bars);
      if (next) {
        // Keep the selected snapshot and its pagination stable through live refreshes.
        selected.bar = next;
        next.setAttribute("aria-expanded", "true");
      } else close();
      if (focused) tab?.focus({preventScroll: true});
    }
  };
}

function createBuildView(request, el, node) {
  const runResults = createBuildResults(request, el, node);
  const periods = createHistoryPeriods(request, el, node);
  let currentBuild, liveVersion = 0, historyVersion = 0;
  let interval = "day", lastChart = "", lastHistoryCheck = 0;
  let submitting = false, waitingForWorker = false;

  const stages = {
    preparing: "Preparing build", jail: "Preparing jail", ports: "Updating ports",
    building: "Building packages", idle: "Idle", error: "Build failed", waiting: "Waiting for worker"
  };
  const triggers = {manual: "Manual request", scheduled: "Scheduled", tracked: "Tracked ports changed", retry: "Retry"};
  const results = {succeeded: "Last run succeeded", failed: "Last run failed", interrupted: "Last run interrupted"};
  const outcomes = ["succeeded", "failed"];
  const fullDate = new Intl.DateTimeFormat(display.locale, {dateStyle: "medium", timeStyle: "short"});
  const utcDate = new Intl.DateTimeFormat(display.locale, {dateStyle: "medium", timeStyle: "medium", timeZone: "UTC"});
  const builderID = new Intl.NumberFormat(display.locale, {minimumIntegerDigits: 2, useGrouping: false});
  const slotsPerPage = 128;
  const pageInput = el("builder-page");
  let builderPage = 1;

  function timeValue(value) {
    if (!value || value.startsWith("0001-01-01")) return null;
    const time = new Date(value);
    return Number.isNaN(time.getTime()) ? null : time;
  }

  function drawBuild(build) {
    const active = build.active ?? ["preparing", "jail", "ports", "building"].includes(build.stage);
    const started = timeValue(build.started), finished = timeValue(build.finished);
    const opening = timeValue(build.window_opens);
    const label = build.pause_error ? "Working hours need attention" : build.paused ? "Build paused" :
      opening && !active ? "Outside working hours" : (!active && results[build.outcome]) || stages[build.stage] || "Worker status unavailable";
    el("build").dataset.stage = build.pause_error ? "error" : build.paused ? "paused" : (build.stage || "waiting");
    el("build-state").textContent = label;
    const progress = build.progress;
    const packageFailure = !active && (build.outcome === "failed" || build.stage === "error") && progress?.failed > 0;
    const message = packageFailure
      ? "Packages that failed to build (" + display.number(progress.failed) + ")"
      : build.message;
    el("build-message").textContent = build.pause_error || (build.paused ? "Package progress is held until the next working window." :
      opening && !active ? "" : message === label ? "" : (message || ""));

    const facts = [];
    function fact(name, value, description) {
      const item = document.createElement("div");
      const term = node("dt", name);
      if (description) {
        item.title = description;
        term.setAttribute("aria-description", description);
      }
      item.append(term, node("dd", value));
      facts.push(item);
    }
    if (started && active) fact("Elapsed", buildDuration(Math.max(0, Date.now() - started) / 1000));
    else if (started && finished && finished >= started) fact("Duration", buildDuration((finished - started) / 1000));
    if (build.trigger && triggers[build.trigger]) fact("Trigger", triggers[build.trigger]);
    if (Number.isInteger(build.origins)) fact("Requested ports", display.number(build.origins),
      "FreeBSD ports on this run’s build list, such as www/firefox. Package progress also counts the dependencies Poudriere builds.");
    if (active && started) fact("Started", fullDate.format(started));
    else if (finished) fact("Finished", fullDate.format(finished));
    el("build-facts").replaceChildren(...facts);

    const known = Number.isInteger(progress?.total) && progress.total >= 0;
    el("build-progress-group").hidden = !known && !(active && build.stage === "building");
    el("build-progress").hidden = known && progress.total === 0;
    if (known && progress.total > 0) {
      const counts = ["built", "fetched", "failed", "skipped", "ignored"].map(key => [key, progress[key] || 0]);
      const completed = counts.reduce((sum, [, count]) => sum + count, 0);
      const bar = el("build-progress");
      bar.max = progress.total;
      bar.value = Math.min(completed, progress.total);
      el("progress-label").textContent = "Processed " + display.number(completed) + " of " + display.number(progress.total);
      el("progress-percent").textContent = display.percent(Math.floor(100 * bar.value / bar.max) / 100);
      el("package-counts").textContent = counts.filter(([, count]) => count > 0).map(([key, count]) => display.number(count) + " " + key).join(" · ");
    } else {
      el("build-progress").removeAttribute("value");
      el("progress-label").textContent = known ? "No packages queued" : "Waiting for package counts";
      el("progress-percent").textContent = "";
      el("package-counts").textContent = "";
    }
    drawJobs(build);

    const when = timeValue(build.next_run);
    const queued = [];
    if (build.retry_pending > 0) {
      queued.push("Ports queued for retry (" + display.number(build.retry_pending) + ")");
    }
    if (build.manual_pending) queued.push("Manual build queued");
    let next, detail = "";
    el("next-build-heading").textContent = build.paused ? "Resumes" : opening ? "Working hours open" : "Next build";
    if (build.paused && build.pause_error && !opening) {
      next = "Retrying resume";
    } else if (opening) {
      // The worker sends the offset in effect at the opening, including across DST changes.
      next = serverTime(build.window_opens, {dateStyle: "medium", timeStyle: "long", hourCycle: "h23"});
      detail = ["Server time", ...queued].join(" · ");
    } else if (queued.length) {
      next = active ? "After this run" : "Queued";
      detail = queued.join(" · ");
    } else if (when) {
      next = fullDate.format(when);
      detail = build.next_reason === "retry" ? "Scheduled retry" : "Scheduled run";
      const minutes = Math.ceil((when - Date.now()) / 60000);
      if (minutes > 0) detail += " · " + relativeTime(minutes * 60);
    } else if (active && build.next_delay_seconds > 0) {
      next = buildDuration(build.next_delay_seconds, "long") + " after this run";
      detail = "Scheduled run";
    } else {
      next = active ? "After this run" : "No schedule available";
      if (active) detail = "The worker sets the next time when this run finishes.";
    }
    el("next-build-time").textContent = next;
    el("next-build-detail").textContent = detail;

    const log = el("build-log-text"), details = el("build-log");
    const reasons = (build.rebuild_reasons || []).join("\n");
    const reasonText = el("build-reasons-text");
    if (reasonText.textContent !== reasons) reasonText.textContent = reasons;
    el("build-reasons").hidden = !reasons;

    const entries = build.log || [];
    // A command failure can be returned separately from the captured output.
    const lines = packageFailure && build.message && !entries.includes(build.message)
      ? [...entries, build.message].join("\n") : entries.join("\n");
    log.hidden = !lines;
    if (log.textContent !== lines) {
      const follow = log.scrollTop + log.clientHeight >= log.scrollHeight - 3;
      log.textContent = lines;
      if (follow && details.open) log.scrollTop = log.scrollHeight;
    }
    details.hidden = !lines && !reasons;
    const button = el("force-build");
    button.disabled = !build.can_force || build.manual_pending || submitting || waitingForWorker;
    button.textContent = submitting ? "Requesting…" : build.manual_pending || waitingForWorker ? "Build queued" : active ? "Queue next build" : opening ? "Queue build" : "Run build now";
    el("worker-mode").hidden = build.can_force !== false;
  }

  function drawJobs(build) {
    const running = el("build-jobs");
    const jobs = new Map((build.jobs || []).map(job => [Number(job.id), job]));
    let capacity = Number.isSafeInteger(build.builder_capacity) && build.builder_capacity > 0 ? build.builder_capacity : 0;
    if (capacity) {
      for (const id of jobs.keys()) capacity = Math.max(capacity, id);
    }
    el("active-jobs").hidden = !jobs.size && !capacity;
    el("job-count").textContent = capacity ? display.number(jobs.size) + " of " + display.number(capacity) : display.number(jobs.size);

    // A legacy snapshot without capacity can identify only the occupied builders.
    const total = capacity || jobs.size;
    const pages = Math.max(1, Math.ceil(total / slotsPerPage));
    const priorPage = builderPage;
    builderPage = Math.min(builderPage, pages);
    pageInput.max = String(pages);
    if (document.activeElement !== pageInput || priorPage !== builderPage) pageInput.value = String(builderPage);
    el("builder-pages").hidden = pages === 1;
    el("builder-previous").disabled = builderPage <= 1;
    el("builder-next").disabled = builderPage >= pages;
    const start = (builderPage - 1) * slotsPerPage;
    const end = Math.min(total, start + slotsPerPage);
    const range = display.number(start + 1) + "–" + display.number(end) + " of " + display.number(total) + " builders";
    if (pages > 1 && el("builder-range").textContent !== range) el("builder-range").textContent = range;

    const focused = running.contains(document.activeElement) && document.activeElement.dataset.logJob ? document.activeElement : null;
    const remaining = new Map(Array.from(running.children, row => [row.dataset.builder, row]));
    let next = running.firstElementChild;
    function drawSlot(id) {
      const job = jobs.get(id);
      const key = String(id);
      const item = remaining.get(key) || document.createElement("li");
      const idle = item.hasAttribute("data-idle");
      remaining.delete(key);
      if (item !== next) running.insertBefore(item, next);
      next = item.nextElementSibling;
      if (!job && idle) return;
      item.tabIndex = -1;
      item.dataset.builder = key;
      item.toggleAttribute("data-idle", !job);
      drawJobRow(item, "Builder " + builderID.format(id), job);
    }
    if (capacity) {
      if (start === 0 && jobs.has(0)) drawSlot(0);
      for (let id = start + 1; id <= end; id++) drawSlot(id);
    } else {
      for (const id of Array.from(jobs.keys()).sort((a, b) => a - b).slice(start, end)) drawSlot(id);
    }
    for (const row of remaining.values()) row.remove();
    if (focused && document.activeElement !== focused) focusRunningLog(focused);
  }

  function chooseBuilderPage(value) {
    if (!currentBuild) return;
    if (Number.isSafeInteger(value)) builderPage = Math.min(Math.max(1, value), Number(pageInput.max));
    pageInput.value = String(builderPage);
    drawJobs(currentBuild);
  }
  el("builder-previous").onclick = () => chooseBuilderPage(builderPage - 1);
  el("builder-next").onclick = () => chooseBuilderPage(builderPage + 1);
  pageInput.onchange = () => chooseBuilderPage(pageInput.valueAsNumber);
  pageInput.onkeydown = event => {
    if (event.key === "Enter") {
      event.preventDefault();
      chooseBuilderPage(pageInput.valueAsNumber);
    }
  };

  function drawJobRow(item, builder, job) {
    const fields = document.createElement("dl");
    fields.className = "job-fields";
    if (item.firstElementChild) item.firstElementChild.replaceWith(fields);
    else item.append(fields);
    function field(label, value, title) {
      const group = document.createElement("div");
      const term = node("dt", label);
      const detail = document.createElement("dd");
      if (title) {
        group.title = title;
        detail.setAttribute("aria-description", title);
      }
      detail.append(value);
      group.append(term, detail);
      fields.append(group);
    }
    const pkg = document.createElement("div");
    pkg.className = "job-package";
    const title = node("strong", job ? job.package || job.origin : "Idle");
    const origin = node("small", [builder, job?.origin].filter(Boolean).join(" · "));
    title.title = title.textContent;
    origin.title = origin.textContent;
    pkg.append(title, origin);
    field("Package", pkg);
    const meter = document.createElement("div");
    meter.className = "job-progress";
    const oldLog = item.querySelector(".log-button");
    if (!job) {
      oldLog?.remove();
      meter.append(node("strong", "—"));
      field("Phase", meter);
      for (const label of ["Elapsed", "CPU", "Memory"]) field(label, "—");
      return;
    }
    const caption = node("strong", job.phase || "—");
    meter.append(caption);
    const measured = Number.isFinite(job.progress_percent);
    function progressBar(label, value, max) {
      const bar = document.createElement("progress");
      bar.setAttribute("aria-label", label + " for " + (job.package || job.origin));
      bar.max = max;
      bar.value = value;
      return bar;
    }
    const previous = job.last_build_seconds, elapsed = job.active_elapsed_seconds;
    const timed = Number.isFinite(previous) && previous > 0 && Number.isFinite(elapsed) && elapsed >= 0;
    if (measured) {
      caption.append(" – " + display.percent(job.progress_percent / 100));
      caption.title = "Progress reported by the build tool";
      meter.append(progressBar("Build tool progress", job.progress_percent, 100));
    } else if (timed && elapsed < previous) {
      caption.append(" – est. " + display.percent(Math.floor(100 * elapsed / previous) / 100));
      const bar = progressBar("Estimated package progress based on the last build time", elapsed, previous);
      bar.className = "estimated-progress";
      meter.append(bar);
    }
    field("Phase", meter);
    let elapsedHint = "Elapsed time since this package started, including pauses";
    if (timed) {
      const remaining = previous - elapsed;
      let prediction = "Previous build time reached";
      if (remaining > 0) prediction = "Est. " + buildDuration(remaining) + (currentBuild.paused ? " left after resume" : " left");
      if (remaining < 0) prediction = buildDuration(-remaining) + " past estimate";
      elapsedHint += ". " + prediction + " · last successful build " + buildDuration(previous);
    }
    field("Elapsed", buildDuration(job.elapsed_seconds), elapsedHint);
    field("CPU", Number.isFinite(job.cpu_percent) ? display.percent(job.cpu_percent / 100) : "—", display.percent(1) + " represents one CPU core");
    field("Memory", Number.isFinite(job.memory_bytes) ? display.unit(job.memory_bytes / 1e6, "megabyte") : "—", "Resident memory used by this builder’s processes");
    if (job.package && timeValue(currentBuild.started)) {
      const sameLog = oldLog?.dataset.logJob === job.package && oldLog.dataset.logRun === currentBuild.started;
      const log = runResults.logButton(currentBuild.started, job, job.phase, sameLog ? oldLog : undefined);
      log.dataset.logJob = job.package;
      log.dataset.logRun = currentBuild.started;
      log.dataset.builder = item.dataset.builder;
      if (!sameLog) {
        oldLog?.remove();
        item.append(log);
      }
    } else oldLog?.remove();
  }

  function receiveBuild(build) {
    ++liveVersion;
    const prior = currentBuild;
    currentBuild = build;
    waitingForWorker = false;
    if (build.manual_pending || prior?.started !== build.started || prior?.finished !== build.finished) {
      el("build-feedback").hidden = true;
    }
    drawBuild(build);
    runResults.update(build);
    el("build-poll-error").hidden = true;
    if (prior && (prior.finished !== build.finished || prior.started !== build.started)) lastHistoryCheck = 0;
  }

  function buildError(detail) {
    el("build-poll-error").textContent = detail;
    el("build-poll-error").hidden = false;
    if (!currentBuild) el("build-state").textContent = "Worker status unavailable";
  }

  async function loadBuild() {
    const version = ++liveVersion;
    try {
      const build = await request("/api/admin/build");
      if (version !== liveVersion) return;
      receiveBuild(build);
    } catch (error) {
      if (version !== liveVersion) return;
      buildError("Could not update build activity. " + error.message);
    }
  }

  el("force-build").onclick = async () => {
    if (!currentBuild?.can_force || submitting || currentBuild.manual_pending || waitingForWorker) return;
    submitting = true;
    drawBuild(currentBuild);
    const feedback = el("build-feedback");
    feedback.hidden = false;
    feedback.textContent = "Sending build request…";
    try {
      const response = await request("/api/admin/build/force", "POST", {});
      if (!response.queued) throw new Error("The worker did not accept the request.");
      waitingForWorker = true;
      feedback.textContent = response.already_queued ? "A manual build is already queued." : "Manual build queued.";
      await loadBuild();
    } catch (error) {
      feedback.textContent = "Build request could not be confirmed. " + error.message;
      await loadBuild();
    } finally {
      submitting = false;
      if (currentBuild) drawBuild(currentBuild);
    }
  };

  // Chart dates match the server's UTC bucket boundaries regardless of the
  // operator's timezone.
  function bucketLabel(date, short) {
    const options = interval === "month" ? {month: short ? "short" : "long", year: short ? "2-digit" : "numeric"} :
      interval === "hour" ? {hour: "numeric", ...(short ? {} : {year: "numeric", month: "short", day: "numeric"})} :
      {month: "short", day: "numeric", ...(short ? {} : {year: "numeric"})};
    const label = date.toLocaleString(display.locale, {...options, timeZone: "UTC"});
    return !short && interval === "week" ? "Week of " + label : label;
  }

  function drawHistory(history) {
    const key = JSON.stringify([interval, history.buckets, history.totals, history.recent]);
    if (key === lastChart) return;
    const buckets = history.buckets;
    const total = history.totals.succeeded;
    const highest = Math.max(1, ...buckets.map(b => outcomes.reduce((n, k) => n + b[k], 0)));
    const step = 10 ** Math.floor(Math.log10(highest));
    const ceiling = Math.ceil(highest / step) * step;
    const range = display.unit(buckets.length, interval, "long");
    el("history-count").textContent = display.number(total);
    el("history-range").textContent = "built in the last " + range + " (UTC)";
    for (const outcome of outcomes) el("total-" + outcome).textContent = display.number(history.totals[outcome]);
    el("chart-max").textContent = display.number(ceiling);
    el("chart-mid").textContent = ceiling % 2 === 0 ? display.number(ceiling / 2) : "";
    el("history-plot").setAttribute("aria-label", display.number(total) + " built and " + display.number(history.totals.failed) + " failed in the last " + range + ", grouped by " + interval + " in UTC.");
    const bars = document.createDocumentFragment();
    const svg = tag => document.createElementNS("http://www.w3.org/2000/svg", tag);
    el("chart-bars").setAttribute("viewBox", "0 0 " + buckets.length * 10 + " " + ceiling);
    for (const [index, bucket] of buckets.entries()) {
      const label = bucketLabel(new Date(bucket.start), false);
      const col = svg("g");
      col.setAttribute("transform", "translate(" + index * 10 + " 0)");
      periods.register(col, bucket, interval, history.as_of, label + " UTC");
      const target = svg("rect");
      target.setAttribute("width", "10");
      target.setAttribute("height", ceiling);
      target.classList.add("chart-target");
      col.append(target);
      let top = ceiling;
      for (const outcome of outcomes) {
        const count = bucket[outcome];
        if (!count) continue;
        top -= count;
        const segment = svg("rect");
        segment.classList.add(outcome);
        segment.setAttribute("x", "1");
        segment.setAttribute("width", "8");
        segment.setAttribute("y", top);
        segment.setAttribute("height", count);
        col.append(segment);
      }
      bars.append(col);
    }
    periods.replace(bars);
    const recent = history.recent || [];
    const attempts = total + history.totals.failed;
    const shown = display.number(recent.length);
    el("history-recent-count").textContent = recent.length < attempts
      ? "(latest " + shown + " of " + display.number(attempts) + ")" : "(" + shown + ")";
    const results = document.createDocumentFragment();
    for (const result of recent) {
      const row = document.createElement("tr");
      const name = node("th", result.package);
      name.scope = "row";
      const time = node("time", utcDate.format(new Date(result.finished)));
      time.dateTime = result.finished;
      const finished = document.createElement("td");
      finished.append(time);
      const action = document.createElement("td");
      if (result.outcome === "failed") {
        const inspect = node("button", "Inspect failure");
        inspect.type = "button";
        inspect.setAttribute("aria-label", "Inspect failure for " + result.package);
        inspect.onclick = () => runResults.reveal(result.started, result.package);
        action.append(inspect);
      } else {
        action.append(runResults.logButton(result.started, result, "Built"));
      }
      row.append(name, node("td", result.outcome === "failed" ? "Failed" : "Built"), finished, action);
      results.append(row);
    }
    el("history-recent-values").replaceChildren(results);
    el("history-recent").hidden = recent.length === 0;
    const ticks = [0, Math.floor((buckets.length - 1) / 2), buckets.length - 1]
      .map(i => node("span", bucketLabel(new Date(buckets[i].start), true)));
    el("chart-ticks").replaceChildren(...ticks);
    el("history-empty").hidden = total + history.totals.failed !== 0;
    lastChart = key;
  }

  async function loadHistory() {
    const version = ++historyVersion;
    const selected = interval;
    const message = el("history-message");
    lastHistoryCheck = Date.now();
    if (el("history-body").hidden) message.textContent = "Loading build history…";
    el("history-retry").hidden = true;
    try {
      const data = await request("/api/admin/build/history", "QUERY", {interval: selected});
      if (version !== historyVersion) return;
      drawHistory(data);
      el("history-body").hidden = false;
      message.textContent = "";
    } catch (error) {
      if (version !== historyVersion) return;
      message.textContent = "Could not update build history. " + error.message;
      el("history-retry").hidden = false;
    }
  }

  for (const button of el("history-interval").querySelectorAll("button")) {
    button.onclick = () => {
      if (interval === button.dataset.interval) return;
      periods.close();
      interval = button.dataset.interval;
      for (const choice of el("history-interval").querySelectorAll("button")) {
        choice.setAttribute("aria-pressed", String(choice === button));
      }
      el("history-body").hidden = true;
      return loadHistory();
    };
  }
  el("history-retry").onclick = loadHistory;

  return {
    loadBuild, loadHistory, receiveBuild, buildError,
    closeResults() {
      periods.close();
      runResults.close();
      ++liveVersion;
      currentBuild = undefined;
      builderPage = 1;
      el("build-jobs").replaceChildren();
      el("active-jobs").hidden = true;
    },
    refreshPeriodic() {
      // Idle streams send only heartbeats. Keep the schedule and elapsed labels current.
      if (currentBuild) drawBuild(currentBuild);
      if (Date.now() - lastHistoryCheck >= 60000) return loadHistory();
    }
  };
}

function createInventoryView(request, el, node) {
  const pageSize = 25;
  const dialog = el("client-dialog");
  const absoluteDate = new Intl.DateTimeFormat(display.locale, {dateStyle: "full", timeStyle: "long"});
  const views = {
    packages: {columns: 8, label: "Entries", renderRow: packageRow, matches: packageMatches},
    clients: {columns: 4, label: "Clients", renderRow: clientRow, matches: (client, q) => matches(q, client.ip)},
    "client-packages": {columns: 3, label: "Packages", renderRow: detailRow, matches: detailMatches}
  };
  let filter = "all", sortKey = "origin", ascending = true;
  let openIP, inflight, detailRequest = 0, clientRequest = 0, clientSearchTimer;

  function megabytes(bytes) {
    if (!Number.isFinite(bytes)) return "—";
    if (bytes > 0 && bytes < 10000) return "<" + display.unit(0.01, "megabyte");
    return display.unit(bytes / 1e6, "megabyte");
  }

  function diskSize(bytes) {
    if (!Number.isFinite(bytes)) return "—";
    const units = ["byte", "kilobyte", "megabyte", "gigabyte", "terabyte", "petabyte"];
    let unit = 0;
    while (bytes >= 1000 && unit < units.length - 1) {
      bytes /= 1000;
      unit++;
    }
    return display.unit(bytes, units[unit]);
  }

  function seenTime(stamp) {
    if (!stamp) return "—";
    const date = new Date(stamp * 1000);
    const label = relativeTime((date.getTime() - Date.now()) / 1000);
    const time = node("time", label);
    time.dateTime = date.toISOString();
    time.title = absoluteDate.format(date);
    time.setAttribute("aria-label", label + ", " + time.title);
    return time;
  }

  function matches(query, ...values) {
    return values.some(value => value && String(value).toLowerCase().includes(query));
  }

  function packageID(p) {
    const version = p.version || p.cache_version;
    return p.name && version ? p.name + "-" + version : p.name || version;
  }

  function packageMatches(p, q) {
    return matches(q, packageID(p), p.origin, p.build_origin, p.cache_key);
  }

  function detailMatches(p, q) {
    return matches(q, p.name, p.origin, p.reported_version, p.requested_version);
  }

  function packageRow(row, p) {
    const name = row.insertCell();
    const origin = p.build_origin || p.origin;
    const pkg = packageID(p);
    name.append(node("strong", origin || pkg || "Unidentified archive"));
    if (origin && pkg) name.append(node("small", pkg));
    else if (!pkg && p.cache_key) {
      const digest = node("small", p.cache_key.slice(0, 12) + "…");
      digest.title = p.cache_key;
      name.append(digest);
    }
    const source = row.insertCell();
    source.textContent = {freebsd: "FreeBSD", poudrine: "Poudrine"}[p.source] || "—";
    if (p.history_only) {
      source.append(node("small", "Build history only"));
      if (p.publication_note) {
        const note = node("small", p.publication_note);
        note.className = "publication-note";
        source.append(note);
      }
      if (p.build_abi) source.append(node("small", p.build_abi));
    } else {
      if (p.worker_fetched) source.append(node("small", "Fetched by Poudriere"));
      if (p.compiled || p.cached) source.append(node("small", p.compiled ? "Compiled" : "Cached"));
    }
    row.insertCell().textContent = p.port_version || "—";
    const versions = row.insertCell();
    versions.append(p.local_version || "—");
    if (p.freebsd_version) versions.append(node("small", "(" + p.freebsd_version + ")"));
    row.insertCell().append(seenTime(p.last_seen));
    row.insertCell().textContent = buildDuration(p.build_seconds);
    row.insertCell().textContent = megabytes(p.size);
    row.insertCell().textContent = Number.isFinite(p.consumers) ? display.number(p.consumers) : "—";
  }

  function clientRow(row, client) {
    row.insertCell().textContent = client.ip;
    row.insertCell().textContent = display.number(client.package_count);
    row.insertCell().append(seenTime(client.last_seen));
    const button = node("button", "View packages");
    button.type = "button";
    button.dataset.client = client.ip;
    button.setAttribute("aria-haspopup", "dialog");
    button.setAttribute("aria-label", "View packages for " + client.ip);
    button.onclick = () => {
      openIP = client.ip;
      el("client-packages-search").value = "";
      views["client-packages"].page = 0;
      el("client-ip").textContent = client.ip;
      dialog.showModal();
      loadClientPackages();
    };
    row.insertCell().append(button);
  }

  function detailRow(row, item) {
    const name = row.insertCell();
    name.append(node("strong", item.name));
    if (item.origin) name.append(node("small", item.origin));
    row.insertCell().textContent = item.requested_version || "—";
    row.insertCell().textContent = item.reported_version || "—";
  }

  function sortPackages() {
    if (!views.packages.records) return;
    const port = p => p.build_origin || p.origin || p.name || p.cache_key || "";
    views.packages.records.sort((a, b) => {
      const byPort = display.collate.compare(port(a), port(b)) || display.collate.compare(a.name || "", b.name || "");
      if (sortKey === "origin") return ascending ? byPort : -byPort;
      const first = a[sortKey], second = b[sortKey];
      if (first == null || second == null) return Number(first == null) - Number(second == null) || byPort;
      return (ascending ? first - second : second - first) || byPort;
    });
  }

  function render(id) {
    const view = views[id];
    if (!view.records && !view.error) return;
    const term = el(id + "-search").value.trim().toLowerCase();
    let records = view.records || [];
    if (id === "packages" && filter !== "all") {
      if (filter === "compiled") records = records.filter(p => p.compiled || p.history_only);
      else if (filter === "stored") records = records.filter(p => p.compiled || p.history_only || p.cached);
      else records = records.filter(p => p[filter]);
    }
    const remote = id === "clients";
    if (term && !remote) records = records.filter(item => view.matches(item, term));
    const pages = Math.ceil(records.length / pageSize);
    if (!remote) view.page = Math.min(view.page, Math.max(0, pages - 1));
    const start = remote ? 0 : view.page * pageSize;
    const summary = !view.records ? "Unavailable" : remote ? view.label : view.label + " (" + display.number(records.length) + ")";
    el(id + "-count").textContent = summary;
    el(id + "-pages").hidden = remote ? view.page === 0 && !view.more : pages < 2;
    el(id + "-page").textContent = "Page " + display.number(view.page + 1) + (remote ? "" : " of " + display.number(pages));
    el(id + "-previous").disabled = view.loading || view.page === 0;
    el(id + "-next").disabled = view.loading || (remote ? !view.more : view.page + 1 >= pages);

    const body = document.createDocumentFragment();
    for (const item of records.slice(start, start + pageSize)) {
      const row = document.createElement("tr");
      view.renderRow(row, item);
      body.append(row);
    }
    if (!records.length) {
      const row = document.createElement("tr");
      const cell = row.insertCell();
      cell.colSpan = view.columns;
      cell.className = "inventory-empty";
      cell.textContent = !view.records ? view.error : term ? "No matches for this search." : view.empty;
      if (view.records && !term && id === "packages" && filter !== "all") {
        if (!view.catalogReady && filter !== "cached") cell.textContent = "Waiting for a verified package catalog.";
        else if (filter === "cached") cell.textContent = "No archives are cached.";
        else if (filter === "compiled") cell.textContent = "No compiled packages are available.";
        else if (filter === "versions_differ") cell.textContent = "No compiled packages have a different version from FreeBSD.";
        else cell.textContent = "No completed builds or cached archives are available.";
      }
      body.append(row);
    }
    const activeClient = id === "clients" ? document.activeElement?.dataset.client : null;
    el(id).replaceChildren(body);
    if (activeClient) focusClient(activeClient);
  }

  function focusClient(ip) {
    const buttons = el("clients").querySelectorAll("button");
    const target = Array.from(buttons).find(button => button.dataset.client === ip) || el("clients-search");
    target.focus({preventScroll: true});
  }

  async function loadClientPackages() {
    const version = ++detailRequest;
    const view = views["client-packages"];
    view.records = undefined;
    view.error = "";
    el("client-dialog-note").textContent = "";
    el("client-packages-count").textContent = "Loading packages…";
    el("client-packages").replaceChildren();
    el("client-packages-pages").hidden = true;
    el("client-packages-retry").hidden = true;
    try {
      const client = await request("/api/admin/clients/packages", "QUERY", {ip: openIP});
      if (version !== detailRequest || !dialog.open) return;
      view.records = client.package_details;
      view.empty = "No requested or reported packages for this client.";
      el("client-dialog-note").textContent = client.reported_at ? "Last installed-package report received " +
        absoluteDate.format(new Date(client.reported_at * 1000)) + "." : "No installed-package report received.";
    } catch {
      if (version !== detailRequest || !dialog.open) return;
      view.error = "Could not load this client’s packages.";
      el("client-packages-retry").hidden = false;
    }
    render("client-packages");
  }

  function showPackages(data) {
    const view = views.packages;
    view.error = data ? "" : "Could not load package inventory.";
    const notes = [];
    if (data) {
      view.records = data.packages;
      view.catalogReady = data.catalog_ready;
      view.empty = data.catalog_ready && data.origins_ready ? "No tracked packages, local builds or cached archives yet." : "No packages in the available inventory.";
      el("inventory-origin-count").textContent = !data.origins_ready && !data.tracked_origins ? "—" :
        (data.origins_ready ? "" : "≥") + display.number(data.tracked_origins);
      el("inventory-cache-count").textContent = display.number(data.cached_count);
      el("inventory-compiled-count").textContent = data.catalog_ready ? display.number(data.packages.filter(p => p.compiled).length) : "—";
      el("inventory-compiled-size").textContent = diskSize(data.compiled_bytes);
      el("inventory-cache-size").textContent = diskSize(data.cached_bytes);
      el("inventory-cache-limit").textContent = Number.isFinite(data.cache_limit) ? "of " + diskSize(data.cache_limit) : "";
      el("inventory-poudriere-size").textContent = diskSize(data.poudriere_bytes);
      if (data.poudriere_bytes == null) notes.push("Poudriere disk usage is unavailable.");
      if (!data.origins_ready) notes.push("The worker’s list of tracked ports is unavailable.");
      if (!data.catalog_ready) notes.push("Catalog versions will appear after a successful catalog refresh.");
      sortPackages();
    } else if (view.records) notes.push("Could not refresh package inventory. Showing the last result.");
    el("packages-note").textContent = notes.join(" ");
    render("packages");
  }

  async function loadClients() {
    const version = ++clientRequest;
    const view = views.clients;
    const page = view.page;
    view.loading = true;
    render("clients");
    try {
      const data = await request("/api/admin/clients/summary", "QUERY", {
        offset: page * pageSize, search: el("clients-search").value.trim()
      });
      if (version !== clientRequest) return;
      view.records = data.clients;
      view.more = data.more;
      view.error = "";
      view.empty = "Clients appear after a repository request or an installed-package report.";
      el("clients-note").textContent = "";
    } catch {
      if (version !== clientRequest) return;
      view.records = undefined;
      view.more = false;
      view.error = "Could not load repository clients.";
      el("clients-note").textContent = view.error;
    } finally {
      if (version === clientRequest) { view.loading = false; render("clients"); }
    }
  }

  for (const [id, view] of Object.entries(views)) {
    view.page = 0;
    el(id + "-search").oninput = () => {
      view.page = 0;
      if (id === "clients") {
        ++clientRequest;
        view.loading = true;
        render(id);
        clearTimeout(clientSearchTimer);
        clientSearchTimer = setTimeout(loadClients, 250);
      } else render(id);
    };
    el(id + "-previous").onclick = () => { view.page--; id === "clients" ? loadClients() : render(id); };
    el(id + "-next").onclick = () => { view.page++; id === "clients" ? loadClients() : render(id); };
  }
  for (const button of el("package-filter").querySelectorAll("button")) {
    button.onclick = () => {
      filter = button.dataset.filter;
      for (const choice of el("package-filter").querySelectorAll("button")) {
        choice.setAttribute("aria-pressed", String(choice === button));
      }
      views.packages.page = 0;
      render("packages");
    };
  }
  for (const button of document.querySelectorAll("[data-sort]")) {
    button.onclick = () => {
      const key = button.dataset.sort;
      ascending = sortKey === key ? !ascending : key === "origin";
      sortKey = key;
      for (const header of button.closest("tr").children) header.removeAttribute("aria-sort");
      button.parentElement.setAttribute("aria-sort", ascending ? "ascending" : "descending");
      views.packages.page = 0;
      sortPackages();
      render("packages");
    };
  }
  dialog.addEventListener("close", () => {
    detailRequest++;
    if (!el("dashboard").hidden) focusClient(openIP);
    openIP = undefined;
    views["client-packages"].records = undefined;
    el("client-packages").replaceChildren();
  });
  el("client-packages-retry").onclick = loadClientPackages;
  return {
    refresh() {
      if (!inflight) inflight = Promise.all([
        request("/api/admin/packages").then(showPackages, () => showPackages(null)),
        loadClients()
      ]).finally(() => { inflight = undefined; });
      return inflight;
    },
    closeClient() { if (dialog.open) dialog.close(); }
  };
}

function initManagement() {
  const encoding = new TextEncoder();
  const b64 = bytes => btoa(String.fromCharCode(...new Uint8Array(bytes)))
    .replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
  const unb64 = text => Uint8Array.from(atob(text.replaceAll("-", "+").replaceAll("_", "/")), c => c.charCodeAt(0));
  function message(text, state = "info", retry = false) {
    el("message").textContent = text;
    el("message").dataset.state = state;
    el("retry").hidden = !retry;
  }
  function report(error) {
    message(error.message, "error", true);
  }
  let keys, pendingKey, freshEnrollment = false, registration, activeSubscription;

  // Older installations saved their non-exportable sign-in key in IndexedDB.
  function legacyKey() {
    return new Promise((resolve, reject) => {
      const req = indexedDB.open("poudrine-credentials", 1);
      req.onupgradeneeded = () => req.transaction.abort();
      req.onblocked = () => resolve(null);
      req.onerror = () => resolve(null);
      req.onsuccess = () => {
        const db = req.result;
        if (!db.objectStoreNames.contains("identity")) { db.close(); resolve(null); return; }
        const tx = db.transaction("identity", "readonly");
        const item = tx.objectStore("identity").get("admin");
        tx.oncomplete = () => { db.close(); resolve(item.result); };
        tx.onabort = () => { db.close(); reject(tx.error); };
      };
    });
  }

  async function signedResponse(path, method, value, credential, signal) {
    const body = encoding.encode(JSON.stringify(value));
    const time = String(Math.floor(Date.now() / 1000));
    const nonce = b64(crypto.getRandomValues(new Uint8Array(16)));
    const prefix = encoding.encode(["poudrine-v2", method, path, time, nonce, ""].join("\n"));
    const wire = new Uint8Array(prefix.length + body.length);
    wire.set(prefix);
    wire.set(body, prefix.length);
    const sig = await crypto.subtle.sign("Ed25519", credential.privateKey, wire);
    const headers = {
      "Content-Type": "application/json",
      "X-Poudrine-Time": time, "X-Poudrine-Nonce": nonce, "X-Poudrine-Signature": b64(sig)
    };
    if (signal) headers.Accept = "text/event-stream";
    if (method === "POST" && path === "/api/setup") headers["X-Poudrine-Key"] = b64(await crypto.subtle.exportKey("raw", credential.publicKey));
    let response;
    try {
      // Sign the API route seen by Poudrine. Fetch it through the website's proxy prefix.
      response = await fetch("." + path, {method, headers, body, signal, cache: "no-store", redirect: "error"});
    } catch (error) {
      if (signal?.aborted) throw error;
      throw new Error("Could not reach Poudrine. Check the connection and try again.");
    }
    if (!response.ok) {
      const error = new Error((await response.text()).trim() || "Request failed");
      error.status = response.status;
      throw error;
    }
    return response;
  }

  async function signed(path, method = "QUERY", value = {}, credential = keys) {
    const response = await signedResponse(path, method, value, credential);
    if (response.status !== 204) return response.json();
  }

  function node(tag, text) {
    const element = document.createElement(tag);
    element.textContent = text;
    return element;
  }

  const buildView = createBuildView(signed, el, node);
  const inventoryView = createInventoryView(signed, el, node);
  let busy = false;
  let stream, reconnect, listTimer, retryDelay = 1000;

  const notificationDialog = el("notification-dialog");
  const bell = el("notification-bell");
  let updatingPush = false;

  function setDashboardVisible(visible) {
    el("dashboard").hidden = !visible;
    el("dashboard-links").hidden = !visible;
    bell.hidden = !visible;
    if (!visible && notificationDialog.open) notificationDialog.close();
    if (!visible) {
      inventoryView.closeClient();
      buildView.closeResults();
    }
    syncDashboardUpdates();
  }

  bell.onclick = () => {
    if (!keys || el("dashboard").hidden || notificationDialog.open) return;
    notificationDialog.showModal();
    el("close-notifications").focus();
    preparePush();
  };
  el("close-notifications").onclick = () => notificationDialog.close();
  notificationDialog.addEventListener("close", () => { if (!bell.hidden) bell.focus(); });

  async function preparePush() {
    if (updatingPush) return;
    const button = el("push");
    const feedback = el("push-message");
    button.disabled = true;
    if (!("serviceWorker" in navigator && "PushManager" in window && "Notification" in window)) {
      button.textContent = "Notifications unavailable";
      feedback.textContent = "This browser does not support Web Push.";
      return;
    }
    updatingPush = true;
    try {
      await navigator.serviceWorker.register("sw.js");
      registration = await navigator.serviceWorker.ready;
      const subscription = await registration.pushManager.getSubscription();
      if (subscription) await signed("/api/admin/subscription", "POST", subscription.toJSON());
      activeSubscription = subscription;
      button.textContent = subscription ? "Disable notifications" : "Enable notifications";
      feedback.textContent = subscription ? "Build notifications are on for this browser." : "";
    } catch (error) {
      registration = undefined;
      feedback.textContent = error.message;
      button.textContent = "Retry notifications";
    }
    updatingPush = false;
    button.disabled = false;
  }

  el("push").onclick = async () => {
    if (updatingPush) return;
    if (!registration) return preparePush();
    const button = el("push");
    const feedback = el("push-message");
    button.disabled = true;
    updatingPush = true;
    feedback.textContent = "Updating notification settings…";
    try {
      if (activeSubscription) {
        await signed("/api/admin/subscription", "DELETE", {endpoint: activeSubscription.endpoint});
        await activeSubscription.unsubscribe();
        activeSubscription = null;
        button.textContent = "Enable notifications";
        feedback.textContent = "Build notifications are off for this browser.";
      } else {
        if (await Notification.requestPermission() !== "granted") {
          throw new Error("Allow notifications in this site’s browser settings to receive build updates.");
        }
        const info = await signed("/api/admin/push-key");
        const subscription = await registration.pushManager.getSubscription() ||
          await registration.pushManager.subscribe({userVisibleOnly: true, applicationServerKey: unb64(info.public_key)});
        await signed("/api/admin/subscription", "POST", subscription.toJSON());
        activeSubscription = subscription;
        button.textContent = "Disable notifications";
        feedback.textContent = "Build notifications are on for this browser.";
      }
    } catch (error) { feedback.textContent = error.message; }
    finally { updatingPush = false; button.disabled = false; }
  };

  async function streamBuild() {
    const controller = new AbortController();
    stream = controller;
    try {
      // Fetch supports the per-request signature headers that EventSource cannot send.
      const response = await signedResponse("/api/admin/build/events", "QUERY", {}, keys, controller.signal);
      await readBuildEvents(response, build => {
        if (controller.signal.aborted) return;
        retryDelay = 1000;
        buildView.receiveBuild(build);
      });
      if (!controller.signal.aborted) throw new Error("The build stream closed.");
    } catch (error) {
      if (controller.signal.aborted) return;
      if (error.status === 401 || error.status === 403) {
        buildView.buildError("Live updates could not authenticate. Sign in again with the registered administrator key.");
        return;
      }
      buildView.buildError("Live build updates disconnected. Reconnecting…");
      reconnect = setTimeout(() => {
        reconnect = undefined;
        syncDashboardUpdates();
      }, retryDelay);
      retryDelay = Math.min(30000, retryDelay * 2);
    } finally {
      controller.abort();
      if (stream === controller) stream = undefined;
    }
  }

  function refreshLists() {
    if (keys && !el("dashboard").hidden && !document.hidden) {
      return Promise.all([buildView.refreshPeriodic(), inventoryView.refresh()]);
    }
  }

  function syncDashboardUpdates() {
    if (keys && !el("dashboard").hidden && !document.hidden) {
      if (!listTimer) listTimer = setInterval(refreshLists, 30000);
      if (!stream && !reconnect) streamBuild();
      return;
    }
    clearInterval(listTimer);
    clearTimeout(reconnect);
    listTimer = reconnect = undefined;
    stream?.abort();
    stream = undefined;
    retryDelay = 1000;
  }
  document.addEventListener("visibilitychange", () => {
    syncDashboardUpdates();
    refreshLists();
  });

  const settingsForm = el("settings-form");
  const settingsInputs = Array.from(settingsForm.querySelectorAll("input[name]"));
  const overlayRows = el("package-overlays");
  const firstSettingsPrompt = "Signed in. Review your network addresses or choose Done to keep the defaults.";
  let savedSettings, settingsRevision, settingsDefaults, settingsBusy = false, settingsConflict = false;

  function settingsMessage(text, error = false) {
    el("settings-message").textContent = text;
    el("settings-message").dataset.state = error ? "error" : "info";
  }

  function clearSettingError() {
    const input = settingsForm.querySelector('[aria-invalid="true"]');
    if (!input) return;
    input.setCustomValidity("");
    input.removeAttribute("aria-invalid");
    input.removeAttribute("aria-errormessage");
  }

  function showSettingError(input, detail) {
    el("settings").open = input.closest("details").open = true;
    input.setAttribute("aria-invalid", "true");
    input.setAttribute("aria-errormessage", "settings-message");
    input.setCustomValidity(detail);
    settingsMessage(detail, true);
    input.focus();
    input.reportValidity();
  }

  function inputEdited(input) {
    return input.type === "checkbox" ? input.checked !== input.defaultChecked : input.value !== input.defaultValue;
  }

  function addOverlayRow(origin = "", overlay = {}) {
    const row = el("overlay-template").content.firstElementChild.cloneNode(true);
    row.dataset.savedOrigin = origin;
    const input = key => row.querySelector('[data-overlay="' + key + '"]');
    input("origin").defaultValue = origin;
    input("default_versions").defaultValue = Object.entries(overlay.default_versions || {}).map(([name, version]) => name + "=" + version).join(" ");
    input("options_set").defaultValue = (overlay.options_set || []).join(" ");
    input("options_unset").defaultValue = (overlay.options_unset || []).join(" ");
    row.querySelector("[data-remove-overlay]").onclick = () => {
      row.remove();
      el("add-overlay").focus();
      settingsControls();
    };
    overlayRows.append(row);
    return input("origin");
  }

  el("add-overlay").onclick = () => {
    addOverlayRow().focus();
    settingsControls();
  };

  function readOverlayRows() {
    const overlays = Object.create(null);
    for (const row of overlayRows.children) {
      const field = key => row.querySelector('[data-overlay="' + key + '"]');
      const words = key => field(key).value.trim().split(/\s+/).filter(Boolean);
      const origin = field("origin").value.trim();
      if (!origin || Object.hasOwn(overlays, origin)) {
        showSettingError(field("origin"), "Enter a unique port origin, such as databases/mydumper.");
        return;
      }
      const overlay = {};
      for (const key of ["options_set", "options_unset"]) {
        const options = words(key);
        if (options.length) overlay[key] = options;
      }
      for (const pair of words("default_versions")) {
        const [name, version, extra] = pair.split("=");
        overlay.default_versions ??= Object.create(null);
        if (!name || !version || extra !== undefined || Object.hasOwn(overlay.default_versions, name)) {
          showSettingError(field("default_versions"), "Use one name=version entry per dependency, such as mysql=12.3m.");
          return;
        }
        overlay.default_versions[name] = version;
      }
      overlays[origin] = overlay;
    }
    return overlays;
  }

  function settingsEdited() {
    if (!savedSettings) return false;
    const rows = Array.from(overlayRows.children);
    return settingsInputs.some(inputEdited) || rows.length !== Object.keys(savedSettings.package_overlays || {}).length ||
      rows.some(row => !row.dataset.savedOrigin || Array.from(row.querySelectorAll("input")).some(inputEdited));
  }

  function settingsControls() {
    const edited = settingsEdited();
    el("settings-fields").disabled = !savedSettings || settingsBusy;
    el("settings-save").disabled = !edited || settingsBusy || settingsConflict;
    el("settings-close").disabled = settingsBusy;
    el("settings-close").textContent = edited ? "Discard edits" : "Done";
  }

  function fillSettings(values, defaults) {
    savedSettings = values;
    settingsDefaults = defaults;
    overlayRows.replaceChildren();
    for (const [origin, overlay] of Object.entries(values.package_overlays || {})) addOverlayRow(origin, overlay);
    for (const input of settingsInputs) {
      const value = values[input.name];
      if (input.type === "checkbox") input.defaultChecked = value;
      else input.defaultValue = value === "" ? defaults[input.name] ?? "" : value ?? "";
    }
    settingsForm.reset();
    clearSettingError();
  }

  async function syncSettings(update) {
    if (settingsBusy) return;
    settingsBusy = true;
    settingsControls();
    const reload = el("settings-reload");
    reload.hidden = true;
    settingsMessage(update ? "Saving settings…" : "Loading settings…");
    let saved = false, invalidInput, detail;
    try {
      if (update) {
        const reply = await signed("/api/admin/config", "PUT", {settings: update, revision: settingsRevision});
        settingsRevision = reply.revision;
        fillSettings(update, settingsDefaults);
        saved = true;
      }
      const result = await signed("/api/admin/config");
      fillSettings(result.settings, {web_url: new URL(".", document.baseURI).href, ...result.defaults});
      settingsRevision = result.revision;
      settingsConflict = false;
      const clock = serverTime(result.server_time_at, {hour: "2-digit", minute: "2-digit", hourCycle: "h23", timeZoneName: "shortOffset"}) || result.server_time;
      el("server-clock").textContent = clock ? " (" + clock + " when loaded)" : "";
      settingsMessage(result.active === false ? "Settings saved. Poudrine has yet to apply them. Check the service log if this persists." : saved ? "Settings saved." : "");
    } catch (error) {
      const conflict = update && (error.status === 409 || error.status === 412);
      if (conflict) settingsConflict = true;
      const field = update && error.status === 400 && /^(?:invalid configuration: )?([a-z_]+): (.+)$/s.exec(error.message);
      invalidInput = field && settingsForm.elements.namedItem(field[1]);
      if (field && field[1] === "package_overlays") {
        const inputs = Array.from(overlayRows.querySelectorAll('[data-overlay="origin"]'));
        invalidInput = inputs.find(input => field[2].startsWith(input.value.trim() + ":")) || inputs[0];
      }
      if (invalidInput) detail = field[2];
      else settingsMessage(saved ? "Settings saved. Reconnect at the configured website address to reload them." :
        conflict ? "Settings changed elsewhere. Reload the latest values before saving." : error.message, true);
      if (!update || saved || conflict) {
        reload.textContent = settingsEdited() ? "Discard edits and reload" : "Reload settings";
        reload.hidden = false;
      }
    } finally {
      settingsBusy = false;
      settingsControls();
      if (invalidInput) showSettingError(invalidInput, detail);
    }
  }

  el("settings").addEventListener("toggle", () => {
    if (el("settings").open && !settingsEdited()) syncSettings();
    if (!el("settings").open && el("message").textContent === firstSettingsPrompt) message("");
  });
  el("settings-reload").onclick = () => syncSettings();
  el("settings-close").onclick = () => {
    if (settingsEdited()) {
      if (settingsConflict) { syncSettings(); return; }
      fillSettings(savedSettings, settingsDefaults);
      settingsMessage("Edits discarded.");
      settingsControls();
    } else {
      el("settings").open = false;
      el("settings").querySelector("summary").focus();
    }
  };
  settingsForm.oninput = () => {
    clearSettingError();
    if (!settingsConflict) settingsMessage("");
    settingsControls();
  };

  settingsForm.onsubmit = event => {
    event.preventDefault();
    if (!settingsEdited() || settingsBusy || settingsConflict) return;
    clearSettingError();
    const invalid = settingsInputs.find(input => !input.validity.valid);
    if (invalid) { showSettingError(invalid, invalid.validationMessage); return; }
    const overlays = readOverlayRows();
    if (!overlays) return;
    const settings = {...savedSettings, package_overlays: overlays};
    // Preserve implicit defaults so URLs, paths and the host release can follow later changes.
    for (const input of settingsInputs.filter(inputEdited)) {
      let value = input.value.trim();
      if (input.inputMode === "url" && value && !/^[a-z][a-z\d+.-]*:\/\//i.test(value)) value = "https://" + value;
      settings[input.name] = input.type === "checkbox" ? input.checked : input.type === "number" ? input.valueAsNumber : value;
    }
    if (Boolean(settings.tls_cert) !== Boolean(settings.tls_key)) {
      const missing = settings.tls_cert ? "tls_key" : "tls_cert";
      showSettingError(settingsForm.elements.namedItem(missing), "Enter both custom PEM paths or clear both to use the generated certificate.");
      return;
    }
    return syncSettings(settings);
  };

  async function openDashboard(enrolledNow = false) {
    el("signin").hidden = true;
    el("private-key").value = "";
    el("logout").hidden = false;
    setDashboardVisible(true);
    // The stream sends the initial build snapshot as soon as it connects.
    try {
      await Promise.all([buildView.loadHistory(), inventoryView.refresh()]);
      message("");
    } catch (error) { report(error); }
    if (enrolledNow) {
      freshEnrollment = false;
      el("settings").open = true;
      el("settings").querySelector("summary").focus();
      message(firstSettingsPrompt);
    }
  }

  async function initialize() {
    if (!window.isSecureContext || !window.crypto?.subtle) {
      message("Open Management over HTTPS or through a localhost SSH tunnel to use the browser key.", "error");
      return;
    }
    const enrolled = document.body.dataset.enrolled === "true";
    if (enrolled) {
      const credential = await legacyKey().catch(() => null);
      el("legacy-login").hidden = !credential?.privateKey;
    }
    el("enrollment").hidden = enrolled;
    el("signin").hidden = !enrolled;
    message("");
  }

  async function reload() {
    if (busy) return;
    busy = true;
    el("refresh").disabled = true;
    el("retry").disabled = true;
    el("dashboard").setAttribute("aria-busy", "true");
    message("Checking repository data…");
    try {
      if (el("dashboard").hidden) await initialize();
      else {
        await Promise.all([buildView.loadBuild(), buildView.loadHistory(), inventoryView.refresh()]);
        message("");
      }
    } catch (error) { report(error); }
    finally {
      busy = false;
      el("refresh").disabled = false;
      el("retry").disabled = false;
      el("dashboard").removeAttribute("aria-busy");
    }
  }
  el("refresh").onclick = reload;
  el("retry").onclick = reload;
  async function newAdministratorKey() {
    const pair = await crypto.subtle.generateKey("Ed25519", true, ["sign", "verify"]);
    const der = new Uint8Array(await crypto.subtle.exportKey("pkcs8", pair.privateKey));
    const base64 = btoa(String.fromCharCode(...der));
    const pem = ["-----BEGIN PRIVATE KEY-----", ...base64.match(/.{1,64}/g), "-----END PRIVATE KEY-----"].join("\n");
    return {...pair, pem};
  }

  async function privateCredential(text) {
    // Password inputs and credential managers can flatten all PEM line breaks.
    const format = /^-----BEGIN PRIVATE KEY-----([A-Za-z0-9+/=\s]+)-----END PRIVATE KEY-----$/;
    const match = text.trim().match(format);
    if (match) {
      try {
        const bytes = Uint8Array.from(atob(match[1].replace(/\s/g, "")), c => c.charCodeAt(0));
        return {privateKey: await crypto.subtle.importKey("pkcs8", bytes, "Ed25519", false, ["sign"])};
      } catch { /* Display the expected format below. */ }
    }
    throw new Error("Paste the complete Ed25519 private key, including its BEGIN and END PRIVATE KEY lines.");
  }

  el("private-key").addEventListener("paste", event => {
    const pasted = event.clipboardData?.getData("text/plain");
    if (!pasted || !/[\r\n]/.test(pasted)) return;
    // Some single-line controls stop a paste at the first newline.
    event.preventDefault();
    const field = event.currentTarget;
    field.setRangeText(pasted.replace(/[\r\n]/g, ""), field.selectionStart, field.selectionEnd, "end");
    field.dispatchEvent(new Event("input", {bubbles: true}));
  });

  function enrollmentComplete() {
    document.body.dataset.enrolled = "true";
    el("token").value = "";
    el("enrollment").hidden = true;
    freshEnrollment = true;
    el("generated-key").value = pendingKey.pem;
    el("key-backup").hidden = false;
    el("copy-key").focus();
    message("Your public key is registered.");
  }

  function protectPendingKey(event) {
    event.preventDefault();
    event.returnValue = true;
  }

  el("token").addEventListener("input", () => el("token").removeAttribute("aria-invalid"));
  el("enroll").onsubmit = async event => {
    event.preventDefault();
    if (busy) return;
    const token = el("token");
    const value = token.value.trim();
    if (!value) {
      token.setAttribute("aria-invalid", "true");
      message("Enter the one-time setup token from the server.", "error");
      token.focus();
      return;
    }
    const button = el("enroll").querySelector("button");
    button.disabled = true;
    busy = true;
    message("Registering your administrator key…");
    try {
      pendingKey ||= await newAdministratorKey();
      window.addEventListener("beforeunload", protectPendingKey);
      await signed("/api/setup", "POST", {token: value}, pendingKey);
      enrollmentComplete();
    } catch (error) {
      if (error.status === 403) {
        window.removeEventListener("beforeunload", protectPendingKey);
        token.setAttribute("aria-invalid", "true");
        message("The setup token was rejected. Copy the current token from the server and try again.", "error");
        token.focus();
      } else if (pendingKey) {
        // The server might have registered this key before the response was lost.
        try {
          await signed("/api/admin/login", "POST", {}, pendingKey);
          enrollmentComplete();
        } catch (probe) {
          if (error.status === 409 && probe.status === 401) {
            document.body.dataset.enrolled = "true";
            pendingKey = null;
            window.removeEventListener("beforeunload", protectPendingKey);
            el("enrollment").hidden = true;
            el("signin").hidden = false;
            message("An administrator key is already registered. Sign in with that key.", "error");
          } else {
            message("Could not confirm enrollment. Keep this page open and submit the token again to retry with the same key.", "error");
          }
        }
      } else { message(error.message, "error"); }
    } finally { busy = false; button.disabled = false; }
  };

  el("copy-key").onclick = async () => {
    try {
      await navigator.clipboard.writeText(el("generated-key").value);
      message("Private key copied. Save it before continuing.");
    } catch {
      el("generated-key").focus();
      el("generated-key").select();
      message("Copy the selected private key and save it before continuing.");
    }
  };
  el("key-saved").onclick = () => {
    pendingKey = null;
    el("generated-key").value = "";
    window.removeEventListener("beforeunload", protectPendingKey);
    el("key-backup").hidden = true;
    el("signin").hidden = false;
    message("Sign in with your saved key to finish setup.");
    el("private-key").focus();
  };

  async function login(useLegacy = false) {
    if (busy) return;
    busy = true;
    const field = el("private-key");
    const buttons = el("login").querySelectorAll("button");
    for (const button of buttons) button.disabled = true;
    message("Verifying the signed request…");
    try {
      const credential = useLegacy ? await legacyKey() : await privateCredential(field.value);
      if (!credential?.privateKey) throw new Error("This browser no longer has a saved administrator key.");
      await signed("/api/admin/login", "POST", {}, credential);
      keys = {privateKey: credential.privateKey};
      await openDashboard(freshEnrollment);
    } catch (error) {
      message(error.status === 401 ? "This private key does not match the administrator key registered on this server." : error.message, "error");
      if (!useLegacy) { field.focus(); field.select(); }
    } finally {
      busy = false;
      for (const button of buttons) button.disabled = false;
    }
  }
  el("login").onsubmit = event => { event.preventDefault(); return login(); };
  el("legacy-login").onclick = () => login(true);
  el("logout").onclick = () => {
    keys = null;
    setDashboardVisible(false);
    window.location.reload();
  };
  window.addEventListener("pagehide", () => {
    keys = pendingKey = null;
    el("private-key").value = "";
    el("generated-key").value = "";
    el("token").value = "";
    setDashboardVisible(false);
  });
  window.addEventListener("pageshow", event => {
    if (event.persisted) window.location.reload();
  });

  reload();
}
