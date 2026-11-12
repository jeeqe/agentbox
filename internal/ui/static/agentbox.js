// AgentBox dashboard glue. htmx and its SSE extension do the heavy lifting;
// this file keeps button states in sync, scrolls the log panes, animates
// removed cards and handles the diff modal.
(() => {
	"use strict";

	const isRunning = (status) => status === "running" || status === "queued";

	function syncCard(card) {
		const meta = card.querySelector("[data-task-status]");
		const status = (meta && meta.dataset.taskStatus) || card.dataset.status || "";
		card.dataset.status = status;
		const live = isRunning(status);
		card.querySelectorAll("[data-merge-action]").forEach((b) => (b.disabled = live));
		card.querySelectorAll("[data-kill-action]").forEach((b) => (b.disabled = !live));
		card.querySelectorAll("[data-restart-action]").forEach((b) => (b.disabled = live));
	}

	// In a race the first agent to pass gets the trophy. Later finishers never
	// overtake it, so this is safe to re-run after every update.
	function markFastest(run) {
		let best = null;
		let bestMs = Infinity;
		run.querySelectorAll("[data-task-card]").forEach((card) => {
			const meta = card.querySelector("[data-task-status]");
			const ms = meta ? Number(meta.dataset.finishedMs) : 0;
			if (meta && meta.dataset.taskStatus === "passed" && ms > 0 && ms < bestMs) {
				best = card;
				bestMs = ms;
			}
		});
		run.querySelectorAll("[data-task-card]").forEach((card) => {
			const badge = card.querySelector(".winner-badge");
			if (card !== best && badge) badge.remove();
			if (card === best && !badge) {
				const right = card.querySelector(".task-header-right");
				const el = document.createElement("span");
				el.className = "winner-badge";
				el.title = "Fastest passing agent";
				el.textContent = "\u{1F3C6} fastest";
				if (right) right.prepend(el);
			}
		});
	}

	function refresh() {
		document.querySelectorAll("[data-task-card]").forEach(syncCard);
		document.querySelectorAll('.run[data-run-kind="race"]').forEach(markFastest);
		document.querySelectorAll(".terminal-output").forEach((pane) => {
			const wait = pane.querySelector(".terminal-wait");
			if (wait && pane.querySelector(".terminal-line")) wait.remove();
			pane.scrollTop = pane.scrollHeight;
		});
	}

	function removeCard(id) {
		const card = document.getElementById("task-card-" + id);
		if (!card) return;
		card.classList.add("is-removing");
		setTimeout(() => card.remove(), 240);
	}

	document.addEventListener("DOMContentLoaded", refresh);
	document.addEventListener("htmx:afterSettle", refresh);

	// After a merge or discard the server lists the cards to drop in a header.
	// Read it in beforeSwap, while the clicked button is still in the page.
	document.addEventListener("htmx:beforeSwap", (e) => {
		const xhr = e.detail && e.detail.xhr;
		const ids = xhr && xhr.getResponseHeader("X-Agentbox-Remove");
		if (!ids) return;
		setTimeout(() => ids.split(",").forEach(removeCard), 60);
	});

	const modalRoot = () => document.getElementById("modal-root");
	document.addEventListener("click", (e) => {
		if (e.target.closest("[data-close-modal]") || e.target.matches("[data-diff-modal]")) {
			modalRoot()?.replaceChildren();
		}
	});
	document.addEventListener("keydown", (e) => {
		if (e.key === "Escape") modalRoot()?.replaceChildren();
	});
})();
