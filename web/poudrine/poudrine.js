"use strict";

function el(id) {
  return document.getElementById(id);
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
    element.textContent = time.toLocaleString(undefined, {dateStyle: "medium", timeStyle: "short"});
  }

  function showCatalog(page) {
    const stamp = page.getElementById("catalog-updated").getAttribute("datetime");
    for (const id of ["catalog-packages", "catalog-ahead"]) {
      el(id).textContent = stamp ? Number(page.getElementById(id).textContent).toLocaleString() : "—";
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

function buildDuration(seconds) {
  if (!Number.isFinite(seconds) || seconds < 0) return "—";
  seconds = Math.floor(seconds);
  if (seconds < 60) return seconds + "s";
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return minutes + "m " + seconds % 60 + "s";
  return Math.floor(minutes / 60) + "h " + minutes % 60 + "m";
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

function createBuildIssues(request, el, node) {
  const panel = el("build-issues"), selector = el("issue-run"), list = el("issue-list");
  const status = el("issue-status"), feedback = el("issue-feedback"), retryAll = el("issue-retry-all");
  const dialog = el("package-log-dialog"), output = el("package-log-output"), earlier = el("package-log-earlier");
  const runDate = new Intl.DateTimeFormat(undefined, {dateStyle: "medium", timeStyle: "long", hourCycle: "h23"});
  let view, selected = "", listVersion = 0, focusPackage, worker, submitting = false;
  const latest = el("package-log-latest");
  let logArgs, logStart, logVersion = 0;

  function retryControls() {
    retryAll.disabled = submitting || !worker?.can_force;
    for (const button of list.querySelectorAll("[data-retry]")) button.disabled = retryAll.disabled;
  }

  async function retry(started, pkg) {
    if (submitting || !worker?.can_force) return;
    submitting = true;
    retryControls();
    const target = pkg || "all affected packages";
    feedback.textContent = "Queuing " + target + "…";
    try {
      const args = pkg ? {started, package: pkg} : {started};
      const reply = await request("/api/admin/build/retry", "POST", args);
      if (!reply.queued) throw new Error("The worker did not accept the retry.");
      feedback.textContent = reply.already_queued ? "The pending build includes " + target + "." : "Retry queued for " + target + ".";
      if (worker?.window_opens) feedback.textContent += " The worker will resume during working hours.";
    } catch (error) {
      feedback.textContent = "Could not confirm the retry. " + error.message;
    } finally {
      submitting = false;
      retryControls();
    }
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
        page.start > 0 ? "Showing output from byte " + page.start.toLocaleString() + " of " + page.size.toLocaleString() + "." : "Showing the log from the beginning.";
      output.scrollTop = before == null ? output.scrollHeight : top + output.scrollHeight - height;
    } catch (error) {
      if (version !== logVersion || !dialog.open) return;
      el("package-log-status").textContent = "Could not read this package’s log. " + error.message;
    } finally {
      if (version === logVersion) latest.disabled = earlier.disabled = false;
    }
  }

  function showLog(started, issue, phase) {
    logArgs = {started, package: issue.package};
    logStart = undefined;
    output.replaceChildren();
    earlier.hidden = true;
    latest.disabled = earlier.disabled = false;
    el("package-log-title").textContent = issue.package;
    el("package-log-info").textContent = [issue.origin, phase, runDate.format(new Date(started))].filter(Boolean).join(" · ");
    dialog.showModal();
    loadLog();
  }
  earlier.onclick = () => loadLog();
  latest.onclick = () => loadLog(true);
  el("close-package-log").onclick = () => dialog.close();
  dialog.addEventListener("close", () => {
    if (dialog.open) return;
    ++logVersion;
    logArgs = undefined;
    output.replaceChildren();
  });

  function makeAction(label, action) {
    const button = node("button", label);
    button.type = "button";
    button.onclick = action;
    return button;
  }

  function issueRow(started, issue, children, failed) {
    const item = document.createElement("li");
    item.tabIndex = -1;
    item.dataset.package = issue.package;
    const row = document.createElement("div"), name = document.createElement("div"), actions = document.createElement("div");
    row.className = "issue-row";
    name.className = "issue-name";
    name.append(node("strong", issue.origin), node("small", issue.package));
    const reason = node("p", failed ? [issue.phase ? "Failed during " + issue.phase : "Build failed", issue.reason].filter(Boolean).join(" · ") :
      "Skipped · blocked by " + (issue.blocked_by || "an unrecorded dependency"));
    reason.className = "issue-reason";
    name.append(reason);
    actions.className = "issue-actions";
    if (failed) actions.append(makeAction("View log", () => showLog(started, issue, issue.phase ? "Failed during " + issue.phase : "Failed")));
    const button = makeAction(children.length ? "Retry with dependents" : "Retry package", () => retry(started, issue.package));
    button.dataset.retry = "";
    actions.append(button);
    row.append(name, actions);
    item.append(row);
    if (children.length) {
      const deps = document.createElement("details"), packages = document.createElement("ul");
      deps.className = "issue-deps";
      deps.open = children.length <= 3;
      deps.append(node("summary", children.length.toLocaleString() + (children.length === 1 ? " skipped dependent" : " skipped dependents")));
      function showDependents() {
        if (!deps.open || packages.childElementCount) return;
        const rows = document.createDocumentFragment();
        for (const child of children) rows.append(node("li", child.origin + " · " + child.package));
        packages.append(rows);
      }
      deps.addEventListener("toggle", showDependents);
      showDependents();
      deps.append(packages);
      item.append(deps);
    }
    return item;
  }

  function runOption(started) {
    const option = node("option", started ? runDate.format(new Date(started)) : "Latest run with issues");
    option.value = started;
    return option;
  }

  function render(data) {
    const runs = data.runs || [];
    const options = [runOption("")];
    if (selected && !runs.includes(selected)) options.push(runOption(selected));
    for (const start of runs) options.push(runOption(start));
    selector.replaceChildren(...options);
    selector.value = selected;
    const failures = data.failed || [], skipped = data.skipped || [];
    const failedNames = new Set(failures.map(issue => issue.package));
    const blocked = new Map(), ungrouped = [];
    for (const child of skipped) {
      if (!failedNames.has(child.blocked_by)) { ungrouped.push(child); continue; }
      if (!blocked.has(child.blocked_by)) blocked.set(child.blocked_by, []);
      blocked.get(child.blocked_by).push(child);
    }
    const rows = document.createDocumentFragment();
    for (const issue of failures) rows.append(issueRow(data.started, issue, blocked.get(issue.package) || [], true));
    for (const child of ungrouped) rows.append(issueRow(data.started, child, [], false));
    list.replaceChildren(rows);
    retryAll.hidden = !failures.length && !skipped.length;
    status.textContent = retryAll.hidden ? "No failed or skipped packages were recorded for this selection." :
      "Run started " + runDate.format(new Date(data.started)) + " · " + failures.length.toLocaleString() + " failed · " +
      new Set(skipped.map(s => s.package)).size.toLocaleString() + " skipped";
    retryControls();
  }

  async function refresh() {
    if (!panel.open) return;
    const version = ++listVersion;
    if (!view) status.textContent = "Loading build details…";
    try {
      const data = await request("/api/admin/build/issues", "QUERY", selected ? {started: selected} : {});
      if (version !== listVersion || !panel.open) return;
      if (!view || JSON.stringify(view) !== JSON.stringify(data)) render(data);
      view = data;
      if (focusPackage) {
        const row = Array.from(list.children).find(item => item.dataset.package === focusPackage);
        (row || selector).focus();
        if (!row) feedback.textContent = "Details for " + focusPackage + " are unavailable in this run.";
        focusPackage = undefined;
      }
    } catch (error) {
      if (version !== listVersion || !panel.open) return;
      view = undefined;
      retryAll.hidden = true;
      list.replaceChildren();
      status.textContent = "Could not load retained build details. " + error.message;
    }
  }

  function chooseRun(started) {
    selected = started;
    view = undefined;
    list.replaceChildren();
    retryAll.hidden = true;
    feedback.textContent = "";
    if (selected && !Array.from(selector.options).some(option => option.value === selected)) {
      selector.add(runOption(started));
    }
    selector.value = selected;
    if (!panel.open) panel.open = true;
    else refresh();
  }
  selector.onchange = () => { focusPackage = undefined; chooseRun(selector.value); };
  panel.addEventListener("toggle", () => { if (panel.open) refresh(); });
  el("issue-refresh").onclick = refresh;
  retryAll.onclick = () => { if (view?.started) return retry(view.started); };

  return {
    showLog,
    reveal(started, pkg) {
      focusPackage = pkg;
      chooseRun(started);
    },
    update(build) {
      const prior = worker;
      worker = build;
      if (prior?.can_force !== build.can_force) retryControls();
      const changedRun = prior?.started !== build.started;
      const failed = build.progress?.failed || 0, skipped = build.progress?.skipped || 0;
      if (!panel.open && (failed || skipped) && (changedRun || !prior?.progress?.failed && !prior?.progress?.skipped)) {
        panel.open = true;
      } else if (panel.open && (changedRun || prior?.finished !== build.finished ||
        failed !== (prior?.progress?.failed || 0) || skipped !== (prior?.progress?.skipped || 0))) {
        refresh();
      }
    },
    close() {
      ++listVersion;
      if (dialog.open) dialog.close();
      panel.open = false;
      focusPackage = view = worker = undefined;
      selected = "";
      list.replaceChildren();
      selector.replaceChildren(runOption(""));
      retryAll.hidden = true;
      status.textContent = feedback.textContent = "";
    }
  };
}

function createBuildView(request, el, node) {
  const issues = createBuildIssues(request, el, node);
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
  const ranges = {hour: "24 hours", day: "30 days", week: "12 weeks", month: "12 months"};
  const fullDate = new Intl.DateTimeFormat(undefined, {dateStyle: "medium", timeStyle: "short", hourCycle: "h23"});

  function timeValue(value) {
    if (!value || value.startsWith("0001-01-01")) return null;
    const time = new Date(value);
    return Number.isNaN(time.getTime()) ? null : time;
  }

  function delayLabel(seconds) {
    const hours = Math.floor(seconds / 3600);
    const minutes = Math.floor(seconds % 3600 / 60);
    const parts = [];
    if (hours) parts.push(hours + (hours === 1 ? " hour" : " hours"));
    if (minutes) parts.push(minutes + (minutes === 1 ? " minute" : " minutes"));
    const remainder = seconds % 60;
    if (remainder || !parts.length) parts.push(remainder + (remainder === 1 ? " second" : " seconds"));
    return parts.join(" ");
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
      ? progress.failed.toLocaleString() + (progress.failed === 1 ? " package" : " packages") + " failed to build."
      : build.message;
    el("build-message").textContent = build.pause_error || (build.paused ? "Package progress is held until the next working window." :
      opening && !active ? "" : message === label ? "" : (message || ""));

    const facts = [];
    function fact(name, value) {
      const item = document.createElement("div");
      item.append(node("dt", name), node("dd", value));
      facts.push(item);
    }
    if (started && active) fact("Elapsed", buildDuration(Math.max(0, Date.now() - started) / 1000));
    else if (started && finished && finished >= started) fact("Duration", buildDuration((finished - started) / 1000));
    if (build.trigger && triggers[build.trigger]) fact("Trigger", triggers[build.trigger]);
    if (Number.isInteger(build.origins)) fact(active ? "Origins in this run" : "Origins in last run", build.origins.toLocaleString());
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
      el("progress-label").textContent = completed.toLocaleString() + " of " + progress.total.toLocaleString() + " queued packages processed";
      el("progress-percent").textContent = Math.floor(100 * bar.value / bar.max) + "%";
      el("package-counts").textContent = counts.filter(([, count]) => count > 0).map(([key, count]) => count.toLocaleString() + " " + key).join(" · ");
    } else {
      el("build-progress").removeAttribute("value");
      el("progress-label").textContent = known ? "No packages queued" : "Waiting for package counts";
      el("progress-percent").textContent = "";
      el("package-counts").textContent = "";
    }
    const jobs = build.jobs || [];
    el("active-jobs").hidden = jobs.length === 0;
    el("job-heading").textContent = build.paused ? "Paused packages" : "Running packages";
    const running = el("build-jobs");
    const focusedJob = running.contains(document.activeElement) ? document.activeElement.dataset.logJob : null;
    running.replaceChildren(...jobs.map(jobRow));
    if (focusedJob) Array.from(running.querySelectorAll("[data-log-job]")).find(button => button.dataset.logJob === focusedJob)?.focus({preventScroll: true});

    const when = timeValue(build.next_run);
    const queued = [];
    if (build.retry_pending > 0) {
      queued.push(build.retry_pending.toLocaleString() + (build.retry_pending === 1 ? " port queued for retry" : " ports queued for retry"));
    }
    if (build.manual_pending) queued.push("Manual build queued");
    let next, detail = "";
    el("next-build-heading").textContent = build.paused ? "Resumes" : opening ? "Working hours open" : "Next build";
    if (build.paused && build.pause_error && !opening) {
      next = "Retrying resume";
    } else if (opening) {
      // The worker sends the offset in effect at the opening, including across DST changes.
      next = build.window_opens.slice(0, 16).replace("T", " ") + " UTC" +
        (build.window_opens.endsWith("Z") ? "" : build.window_opens.slice(-6));
      detail = ["Server time", ...queued].join(" · ");
    } else if (queued.length) {
      next = active ? "After this run" : "Queued";
      detail = queued.join(" · ");
    } else if (when) {
      next = fullDate.format(when);
      detail = build.next_reason === "retry" ? "Scheduled retry" : "Scheduled run";
      const minutes = Math.ceil((when - Date.now()) / 60000);
      if (minutes > 0) detail += " · in " + (minutes < 60 ? minutes + "m" : Math.floor(minutes / 60) + "h " + minutes % 60 + "m");
    } else if (active && build.next_delay_seconds > 0) {
      next = delayLabel(build.next_delay_seconds) + " after this run";
      detail = "Scheduled run";
    } else {
      next = active ? "After this run" : "No schedule available";
      if (active) detail = "The worker sets the next time when this run finishes.";
    }
    el("next-build-time").textContent = next;
    el("next-build-detail").textContent = detail;

    const log = el("build-log-text"), details = el("build-log");
    const entries = build.log || [];
    // A command failure can be returned separately from the captured output.
    const lines = packageFailure && build.message && !entries.includes(build.message)
      ? [...entries, build.message].join("\n") : entries.join("\n");
    if (log.textContent !== lines) {
      const follow = log.scrollTop + log.clientHeight >= log.scrollHeight - 3;
      log.textContent = lines;
      if (follow && details.open) log.scrollTop = log.scrollHeight;
    }
    details.hidden = !lines;
    const button = el("force-build");
    button.disabled = !build.can_force || build.manual_pending || submitting || waitingForWorker;
    button.textContent = submitting ? "Requesting…" : build.manual_pending || waitingForWorker ? "Build queued" : active ? "Queue next build" : opening ? "Queue build" : "Run build now";
    el("worker-mode").hidden = build.can_force !== false;
  }

  function jobRow(job) {
    const item = document.createElement("li");
    const pkg = document.createElement("div");
    pkg.className = "job-package";
    pkg.append(node("strong", job.package || job.origin));
    pkg.append(node("small", ["Builder " + job.id, job.origin].filter(Boolean).join(" · ")));
    if (job.package && timeValue(currentBuild.started)) {
      const viewLog = node("button", "View log");
      viewLog.type = "button";
      viewLog.dataset.logJob = job.package;
      viewLog.setAttribute("aria-label", "View build log for " + job.package);
      const started = currentBuild.started;
      viewLog.onclick = () => issues.showLog(started, job, job.phase);
      pkg.append(viewLog);
    }

    const phase = document.createElement("div");
    phase.className = "job-phase";
    const label = node("span", "Phase");
    label.className = "job-phase-label";
    const measured = Number.isFinite(job.progress_percent);
    phase.append(label, node("strong", (job.phase || "—") + (measured ? " · " + job.progress_percent + "%" : "")));
    if (measured) {
      const bar = document.createElement("progress");
      bar.setAttribute("aria-label", "Build tool progress for " + (job.package || job.origin));
      bar.max = 100;
      bar.value = job.progress_percent;
      phase.append(bar);
    }
    const metrics = document.createElement("dl");
    metrics.className = "job-metrics";
    function metric(label, value, title) {
      const group = document.createElement("div");
      const term = node("dt", label);
      term.title = title;
      group.append(term, node("dd", value));
      metrics.append(group);
    }
    metric("Elapsed", buildDuration(job.elapsed_seconds), "Elapsed time since this package started, including pauses");
    metric("CPU", Number.isFinite(job.cpu_percent) ? job.cpu_percent.toLocaleString(undefined, {maximumFractionDigits: 1}) + "%" : "—", "100% represents one CPU core");
    metric("Memory", Number.isFinite(job.memory_bytes) ? (job.memory_bytes / 1048576).toLocaleString(undefined, {maximumFractionDigits: 1}) + " MiB" : "—", "Resident memory used by this builder’s processes");
    item.append(pkg, phase, metrics);
    return item;
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
    issues.update(build);
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
      drawBuild(currentBuild);
    }
  };

  // Chart dates match the server's UTC bucket boundaries regardless of the
  // operator's timezone. The data table gives exact values for every column.
  function bucketLabel(date, short) {
    const options = interval === "month" ? {month: short ? "short" : "long", year: short ? "2-digit" : "numeric"} :
      interval === "hour" ? {hour: "2-digit", hourCycle: "h23", ...(short ? {} : {year: "numeric", month: "short", day: "numeric"})} :
      {month: "short", day: "numeric", ...(short ? {} : {year: "numeric"})};
    const label = date.toLocaleString(undefined, {...options, timeZone: "UTC"});
    return !short && interval === "week" ? "Week of " + label : label;
  }

  function drawHistory(history) {
    const key = JSON.stringify(history);
    if (key === lastChart) return;
    const buckets = history.buckets;
    const total = history.totals.succeeded;
    const highest = Math.max(1, ...buckets.map(b => outcomes.reduce((n, k) => n + b[k], 0)));
    const step = 10 ** Math.floor(Math.log10(highest));
    const ceiling = Math.ceil(highest / step) * step;
    el("history-count").textContent = total.toLocaleString();
    el("history-range").textContent = (total === 1 ? "package built" : "packages built") + " in the last " + ranges[interval] + " (UTC)";
    for (const outcome of outcomes) el("total-" + outcome).textContent = history.totals[outcome].toLocaleString();
    el("chart-max").textContent = ceiling.toLocaleString();
    el("chart-mid").textContent = ceiling % 2 === 0 ? (ceiling / 2).toLocaleString() : "";
    el("history-plot").setAttribute("aria-label", total.toLocaleString() + " packages built and " + history.totals.failed.toLocaleString() + " failed in the last " + ranges[interval] + ", grouped by " + interval + " in UTC. Exact counts follow in the data table.");
    const bars = document.createDocumentFragment(), rows = document.createDocumentFragment();
    const svg = tag => document.createElementNS("http://www.w3.org/2000/svg", tag);
    el("chart-bars").setAttribute("viewBox", "0 0 " + buckets.length * 10 + " " + ceiling);
    for (const [index, bucket] of buckets.entries()) {
      const label = bucketLabel(new Date(bucket.start), false);
      const col = svg("g");
      col.setAttribute("transform", "translate(" + index * 10 + " 0)");
      const title = svg("title");
      title.textContent = label + " UTC · " + outcomes.map(k => bucket[k] + " " + k).join(", ");
      const target = svg("rect");
      target.setAttribute("width", "10");
      target.setAttribute("height", ceiling);
      target.classList.add("chart-target");
      col.append(title, target);
      const row = document.createElement("tr");
      const th = node("th", label);
      th.scope = "row";
      row.append(th);
      let top = ceiling;
      for (const outcome of outcomes) {
        const count = bucket[outcome];
        row.append(node("td", count.toLocaleString()));
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
      rows.append(row);
    }
    el("chart-bars").replaceChildren(bars);
    el("history-values").replaceChildren(rows);
    const recent = history.recent || [];
    const attempts = total + history.totals.failed;
    const shown = recent.length.toLocaleString();
    el("history-recent-count").textContent = recent.length < attempts
      ? "(latest " + shown + " of " + attempts.toLocaleString() + ")" : "(" + shown + ")";
    const results = document.createDocumentFragment();
    for (const result of recent) {
      const row = document.createElement("tr");
      const name = node("th", result.package);
      name.scope = "row";
      const time = node("time", result.finished.replace("T", " ").slice(0, 19));
      time.dateTime = result.finished;
      const finished = document.createElement("td");
      finished.append(time);
      const outcome = node("td", "Built");
      if (result.outcome === "failed") {
        const inspect = node("button", "Inspect failure");
        inspect.type = "button";
        inspect.setAttribute("aria-label", "Inspect failure for " + result.package);
        inspect.onclick = () => issues.reveal(result.started, result.package);
        outcome.replaceChildren(inspect);
      } else {
        const log = node("button", "View log");
        log.type = "button";
        log.setAttribute("aria-label", "View build log for " + result.package);
        log.onclick = () => issues.showLog(result.started, result, "Built");
        outcome.append(" ", log);
      }
      row.append(name, outcome, finished);
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
    loadBuild, loadHistory, receiveBuild, buildError, closeIssues: issues.close,
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
  const absoluteDate = new Intl.DateTimeFormat(undefined, {dateStyle: "full", timeStyle: "long"});
  const collate = new Intl.Collator(undefined, {numeric: true});
  const views = {
    packages: {columns: 8, unit: "entry", renderRow: packageRow, matches: packageMatches},
    clients: {columns: 4, unit: "client", renderRow: clientRow, matches: (client, q) => matches(q, client.ip)},
    "client-packages": {columns: 3, unit: "package", renderRow: detailRow, matches: detailMatches}
  };
  let filter = "all", sortKey = "origin", ascending = true;
  let openIP, inflight, detailRequest = 0;

  function quantity(n, unit) {
    const plural = unit === "entry" ? "entries" : unit + "s";
    return n.toLocaleString() + " " + (n === 1 ? unit : plural);
  }

  function megabytes(bytes) {
    if (!Number.isFinite(bytes)) return "—";
    if (bytes > 0 && bytes < 10000) return "<0.01";
    return (bytes / 1e6).toLocaleString(undefined, {maximumFractionDigits: 2});
  }

  function diskSize(bytes) {
    if (!Number.isFinite(bytes)) return "—";
    const units = ["B", "kB", "MB", "GB", "TB", "PB", "EB"];
    let unit = 0;
    while (bytes >= 1000 && unit < units.length - 1) {
      bytes /= 1000;
      unit++;
    }
    return bytes.toLocaleString(undefined, {maximumFractionDigits: 2}) + " " + units[unit];
  }

  function seenTime(stamp) {
    if (!stamp) return "—";
    const date = new Date(stamp * 1000);
    const seconds = Math.max(0, Math.floor((Date.now() - date.getTime()) / 1000));
    let label = "Just now";
    for (const [duration, unit] of [[86400, "day"], [3600, "hour"], [60, "minute"]]) {
      if (seconds >= duration) {
        label = quantity(Math.floor(seconds / duration), unit) + " ago";
        break;
      }
    }
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
      const state = node("small", "Build history only");
      state.title = "Poudriere built this version. The current catalog has no matching compiled archive.";
      source.append(state);
      if (p.build_abi) source.append(node("small", p.build_abi));
    } else if (p.compiled || p.cached) source.append(node("small", p.compiled ? "Compiled" : "Cached"));
    row.insertCell().textContent = p.port_version || "—";
    const versions = row.insertCell();
    versions.append(p.local_version || "—");
    if (p.freebsd_version) versions.append(node("small", "(" + p.freebsd_version + ")"));
    row.insertCell().append(seenTime(p.last_seen));
    row.insertCell().textContent = buildDuration(p.build_seconds);
    row.insertCell().textContent = megabytes(p.size);
    row.insertCell().textContent = p.consumers?.toLocaleString() ?? "—";
  }

  function clientRow(row, client) {
    row.insertCell().textContent = client.ip;
    row.insertCell().textContent = client.package_count.toLocaleString();
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
      const byPort = collate.compare(port(a), port(b)) || collate.compare(a.name || "", b.name || "");
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
    if (term) records = records.filter(item => view.matches(item, term));
    const pages = Math.ceil(records.length / pageSize);
    view.page = Math.min(view.page, Math.max(0, pages - 1));
    const start = view.page * pageSize;
    const summary = !view.records ? "Unavailable" : quantity(records.length, view.unit);
    el(id + "-count").textContent = summary;
    el(id + "-pages").hidden = pages < 2;
    el(id + "-page").textContent = "Page " + (view.page + 1) + " of " + pages;
    el(id + "-previous").disabled = view.page === 0;
    el(id + "-next").disabled = view.page + 1 >= pages;

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
        (data.origins_ready ? "" : "≥") + data.tracked_origins.toLocaleString();
      el("inventory-cache-count").textContent = data.cached_count.toLocaleString();
      el("inventory-compiled-count").textContent = data.catalog_ready ? data.packages.filter(p => p.compiled).length.toLocaleString() : "—";
      el("inventory-compiled-size").textContent = diskSize(data.compiled_bytes);
      el("inventory-cache-size").textContent = diskSize(data.cached_bytes);
      el("inventory-cache-limit").textContent = Number.isFinite(data.cache_limit) ? "of " + diskSize(data.cache_limit) : "";
      el("inventory-poudriere-size").textContent = diskSize(data.poudriere_bytes);
      if (data.poudriere_bytes == null) notes.push("Poudriere disk usage is unavailable.");
      if (!data.origins_ready) notes.push("The worker’s list of tracked origins is unavailable.");
      if (!data.catalog_ready) notes.push("Catalog versions will appear after a successful catalog refresh.");
      sortPackages();
    } else if (view.records) notes.push("Could not refresh package inventory. Showing the last result.");
    el("packages-note").textContent = notes.join(" ");
    render("packages");
  }

  function showClients(records) {
    const view = views.clients;
    view.error = records ? "" : "Could not load repository clients.";
    if (records) {
      view.records = records.sort((a, b) => b.last_seen - a.last_seen || collate.compare(a.ip, b.ip));
      view.empty = "Clients appear after a repository request or an installed-package report.";
    }
    el("clients-note").textContent = !records && view.records ? "Could not refresh clients. Showing the last result." : "";
    render("clients");
  }

  for (const [id, view] of Object.entries(views)) {
    view.page = 0;
    el(id + "-search").oninput = () => { view.page = 0; render(id); };
    el(id + "-previous").onclick = () => { view.page--; render(id); };
    el(id + "-next").onclick = () => { view.page++; render(id); };
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
        request("/api/admin/clients/summary").then(showClients, () => showClients(null))
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
    bell.hidden = !visible;
    if (!visible && notificationDialog.open) notificationDialog.close();
    if (!visible) {
      inventoryView.closeClient();
      buildView.closeIssues();
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

  function settingsEdited() {
    return savedSettings && settingsInputs.some(inputEdited);
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
      el("server-clock").textContent = result.server_time ? " (" + result.server_time + " when loaded)" : "";
      settingsMessage(result.active === false ? "Settings saved. Poudrine has yet to apply them. Check the service log if this persists." : saved ? "Settings saved." : "");
    } catch (error) {
      const conflict = update && (error.status === 409 || error.status === 412);
      if (conflict) settingsConflict = true;
      const field = update && error.status === 400 && /^(?:invalid configuration: )?([a-z_]+): (.+)$/s.exec(error.message);
      invalidInput = field && settingsForm.elements.namedItem(field[1]);
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
      settingsForm.reset();
      clearSettingError();
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
    const settings = {...savedSettings};
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
