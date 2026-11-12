// AgentBox dashboard glue. htmx and its SSE extension do the live updates;
// this file lays out the timeline, keeps the summary and the "first to pass"
// marker current, and handles the output and diff drawers.
(() => {
	"use strict";

	const isRunning = (status) => status === "running" || status === "queued";
	const pad = (n) => String(n).padStart(2, "0");

	function clock(secs) {
		secs = Math.max(0, Math.floor(secs));
		const h = Math.floor(secs / 3600);
		const m = Math.floor((secs % 3600) / 60);
		return h ? `${h}:${pad(m)}:${pad(secs % 60)}` : `${m}:${pad(secs % 60)}`;
	}

	const metaOf = (lane) => lane.querySelector("[data-task-status]");

	// The status stream replaces only the meta block, so the lane's own
	// data-status is copied from it. The stylesheet keys off data-status.
	function syncStatus(lane) {
		const meta = metaOf(lane);
		lane.dataset.status = (meta && meta.dataset.taskStatus) || lane.dataset.status || "";
	}

	function elapsedSeconds(lane, now) {
		const meta = metaOf(lane);
		const started = meta ? Number(meta.dataset.startedMs) : 0;
		if (!started) return 0;
		const finished = Number(meta.dataset.finishedMs) || 0;
		return Math.max(0, ((finished || now) - started) / 1000);
	}

	function drawAxis(run, axis) {
		const el = run.querySelector("[data-axis]");
		if (!el || el.dataset.drawn === String(axis)) return;
		el.dataset.drawn = String(axis);
		const step = axis <= 180 ? 30 : axis <= 600 ? 60 : 300;
		el.replaceChildren();
		for (let t = 0; t < axis; t += step) {
			const tick = document.createElement("i");
			tick.style.left = (t / axis) * 100 + "%";
			tick.textContent = clock(t);
			el.append(tick);
		}
	}

	// In a race the first agent to pass is the natural pick. Later finishers
	// never overtake it, so this is safe to re-run after every update.
	function markFirst(run) {
		if (run.dataset.runKind !== "race") return;
		let best = null;
		let bestMs = Infinity;
		run.querySelectorAll("[data-task-card]").forEach((lane) => {
			const meta = metaOf(lane);
			const ms = meta ? Number(meta.dataset.finishedMs) : 0;
			if (meta && meta.dataset.taskStatus === "passed" && ms > 0 && ms < bestMs) {
				best = lane;
				bestMs = ms;
			}
		});
		run.querySelectorAll("[data-task-card]").forEach((lane) => lane.classList.toggle("is-first", lane === best));
	}

	function layoutRun(run, now) {
		const lanes = [...run.querySelectorAll("[data-task-card]")];
		lanes.forEach(syncStatus);
		const times = lanes.map((lane) => elapsedSeconds(lane, now));
		const axis = Math.max(60, Math.ceil((Math.max(0, ...times) * 1.1) / 30) * 30);
		run.style.setProperty("--axis", axis);
		lanes.forEach((lane, i) => {
			lane.style.setProperty("--p", (times[i] / axis).toFixed(4));
			if (!isRunning(lane.dataset.status)) return;
			const time = lane.querySelector("[data-clock]");
			if (time) time.textContent = clock(times[i]);
			// While an agent runs, its latest output line is the second line.
			const tick = lane.querySelector("[data-tick]");
			const last = lane.querySelector(".log-output")?.lastElementChild;
			if (tick && last) tick.textContent = last.textContent;
		});
		drawAxis(run, axis);
		markFirst(run);
	}

	function updateSummary() {
		const el = document.querySelector("[data-summary]");
		if (!el) return;
		const count = { total: 0, passed: 0, failed: 0, running: 0 };
		document.querySelectorAll("[data-task-card]").forEach((lane) => {
			const s = lane.dataset.status;
			count.total++;
			if (s === "passed") count.passed++;
			else if (s === "failed" || s === "conflict") count.failed++;
			else if (isRunning(s)) count.running++;
		});
		el.textContent = `${count.total} agents, ${count.passed} passed, ${count.failed} failed, ${count.running} running`;
	}

	function keepLogsPinned() {
		document.querySelectorAll(".log:not([hidden]) .log-output").forEach((pane) => {
			if (pane.dataset.stick !== "false") pane.scrollTop = pane.scrollHeight;
		});
	}

	function refresh() {
		const now = Date.now();
		document.querySelectorAll(".run").forEach((run) => layoutRun(run, now));
		updateSummary();
		keepLogsPinned();
	}

	function removeLane(id) {
		const lane = document.getElementById("task-card-" + id);
		if (!lane) return;
		lane.classList.add("is-removing");
		setTimeout(() => {
			const run = lane.closest(".run");
			lane.remove();
			if (run && !run.querySelector("[data-task-card]")) run.remove();
			// With nothing left to decide, reload into the empty state, where the
			// finished run now shows under "Earlier runs".
			if (!document.querySelector(".run")) {
				location.reload();
				return;
			}
			refresh();
		}, 240);
	}

	document.addEventListener("DOMContentLoaded", refresh);
	document.addEventListener("htmx:afterSettle", refresh);
	setInterval(() => {
		if (document.querySelector('[data-status="running"], [data-status="queued"]')) refresh();
	}, 1000);

	// After a merge or discard the server lists the lanes to drop in a header.
	// Read it in beforeSwap, while the clicked button is still in the page.
	document.addEventListener("htmx:beforeSwap", (e) => {
		const xhr = e.detail && e.detail.xhr;
		const ids = xhr && xhr.getResponseHeader("X-Agentbox-Remove");
		if (!ids) return;
		setTimeout(() => ids.split(",").forEach(removeLane), 60);
	});

	document.addEventListener("scroll", (e) => {
		const pane = e.target;
		if (pane && pane.classList && pane.classList.contains("log-output")) {
			pane.dataset.stick = String(pane.scrollHeight - pane.scrollTop - pane.clientHeight < 24);
		}
	}, true);

	document.addEventListener("click", (e) => {
		const toggle = e.target.closest("[data-toggle-log]");
		if (toggle) {
			const log = toggle.closest("[data-task-card]").querySelector("[data-log]");
			log.hidden = !log.hidden;
			toggle.setAttribute("aria-expanded", String(!log.hidden));
			toggle.textContent = log.hidden ? "Output" : "Hide output";
			if (!log.hidden) keepLogsPinned();
			return;
		}
		const close = e.target.closest("[data-close-diff]");
		if (close) close.closest(".diff-slot").replaceChildren();
	});
})();
