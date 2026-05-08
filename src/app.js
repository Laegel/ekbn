import { marked } from 'marked';

export function initApp() {
  const board = document.getElementById("board");
  const cardDialog = document.getElementById("card-dialog");
  const cardForm = document.getElementById("card-form");
  const contentInput = document.getElementById("card-content-input");
  const columnDialog = document.getElementById("column-dialog");
  const columnForm = document.getElementById("column-form");
  const mdEditor = document.getElementById("md-editor");
  const categoriesList = document.getElementById("categories-list");
  const categoriesInput = document.getElementById("card-categories-input");
  const commentsList = document.getElementById("comments-list");
  const commentInput = document.getElementById("card-comment-input");
  const addCommentBtn = document.getElementById("add-comment-btn");

  let currentCategories = [];
  let currentColumn = "";
  let currentFilename = "";
  let localMoveInProgress = false;

  marked.setOptions({
    breaks: true,
    gfm: true,
  });

  let editingMarkdown = false;
  let previewEl = null;

  function showMarkdownEdit() {
    editingMarkdown = true;
    contentInput.style.display = "block";
    if (previewEl) {
      previewEl.style.display = "none";
    }
    contentInput.focus();
  }

  function showMarkdownPreview() {
    editingMarkdown = false;
    contentInput.style.display = "none";
    if (!previewEl) {
      previewEl = document.createElement("div");
      previewEl.className = "md-preview";
      previewEl.addEventListener("click", () => showMarkdownEdit());
      mdEditor.appendChild(previewEl);
    }
    const raw = contentInput.value;
    if (raw.trim()) {
      previewEl.className = "md-preview";
      previewEl.innerHTML = marked.parse(raw);
    } else {
      previewEl.className = "md-preview empty";
      previewEl.textContent = "Click to write Markdown...";
    }
    previewEl.style.display = "block";
  }

  contentInput.addEventListener("blur", () => {
    if (editingMarkdown) {
      showMarkdownPreview();
    }
  });

  const es = new EventSource("/api/events");

  es.addEventListener("card-created", (e) => {
    if (localMoveInProgress) return;
    const data = JSON.parse(e.data);
    animateCardCreated(data.column, data.card);
    updateColumnCount(data.column);
  });
  es.addEventListener("card-deleted", (e) => {
    if (localMoveInProgress) return;
    const data = JSON.parse(e.data);
    animateCardDeleted(data.column, data.filename);
    updateColumnCount(data.column);
  });
  es.addEventListener("card-updated", (e) => {
    if (localMoveInProgress) return;
    const data = JSON.parse(e.data);
    animateCardUpdated(data.column, data.card);
  });
  es.addEventListener("card-moved", (e) => {
    if (localMoveInProgress) return;
    const data = JSON.parse(e.data);
    animateCardMove(data.fromColumn, data.toColumn, data.card);
  });
  es.addEventListener("column-reordered", () => {
    if (localMoveInProgress) return;
    loadBoard();
  });

  marked.setOptions({
    breaks: true,
    gfm: true,
  });

  categoriesInput.addEventListener("keydown", (e) => {
    if (e.key === ",") {
      e.preventDefault();
      const val = categoriesInput.value.trim();
      if (val && !currentCategories.includes(val)) {
        currentCategories.push(val);
        renderCategoryPills();
      }
      categoriesInput.value = "";
    }
    if (
      e.key === "Backspace" &&
      !categoriesInput.value &&
      currentCategories.length
    ) {
      currentCategories.pop();
      renderCategoryPills();
    }
  });

  function renderCategoryPills() {
    categoriesList.innerHTML = "";
    currentCategories.forEach((cat, i) => {
      const pill = document.createElement("span");
      pill.className = "category-pill";
      pill.innerHTML = `${escapeHtml(cat)}<button type="button" class="remove-cat" data-index="${i}">&#10005;</button>`;
      categoriesList.appendChild(pill);
    });
    categoriesList.querySelectorAll(".remove-cat").forEach((btn) => {
      btn.addEventListener("click", () => {
        currentCategories.splice(parseInt(btn.dataset.index), 1);
        renderCategoryPills();
      });
    });
  }

  function setCategories(cats) {
    currentCategories = [...cats];
    renderCategoryPills();
    categoriesInput.value = "";
  }

  function getCategories() {
    return [...currentCategories];
  }

  function clearCategories() {
    currentCategories = [];
    renderCategoryPills();
    categoriesInput.value = "";
  }

  let draggedCard = null;
  let draggedColumn = null;
  let columnDragPlaceholder = null;

  async function loadBoard() {
    try {
      const res = await fetch("/api/columns");
      if (!res.ok) throw new Error("Failed to load columns");
      const columns = await res.json();
      renderBoard(columns);
    } catch (err) {
      console.error(err);
    }
  }

  function renderBoard(columns) {
    board.innerHTML = "";
    columns.forEach((col) => {
      const colEl = document.createElement("div");
      colEl.className = "column card card-sm bg-base-200 p-4 min-w-[300px] max-w-[300px] flex-shrink-0";
      colEl.dataset.column = col.id;
      colEl.dataset.slug = col.slug;

      colEl.innerHTML = `
              <div class="card-body p-0">
                  <div class="flex items-center justify-between mb-4 cursor-move" draggable="true">
                      <h2 class="text-lg font-semibold">${escapeHtml(col.name)} <span class="column-count text-base-content/60 font-normal">(${col.cards.length})</span></h2>
                      <div class="card-actions">
                          <button class="btn btn-primary btn-xs" data-column="${col.id}" title="Add card">+</button>
                      </div>
                  </div>
                  <div class="column-cards min-h-[200px] space-y-2" data-column="${col.id}"></div>
                  <div class="card-actions justify-center mt-4">
                      <button class="btn btn-ghost btn-sm w-full add-card-btn" data-column="${col.id}">+ Add card</button>
                  </div>
              </div>
          `;

      const cardsContainer = colEl.querySelector(".column-cards");
      col.cards.forEach((card) => {
        cardsContainer.appendChild(createCardElement(card, col.id));
      });

      cardsContainer.addEventListener("dragover", handleDragOver);
      cardsContainer.addEventListener("dragleave", handleDragLeave);
      cardsContainer.addEventListener("drop", handleDrop);

      const header = colEl.querySelector("[draggable]");
      header.addEventListener("dragstart", handleColumnDragStart);
      header.addEventListener("dragend", handleColumnDragEnd);

      colEl.addEventListener("dragover", handleColumnDragOver);
      colEl.addEventListener("dragenter", handleColumnDragEnter);
      colEl.addEventListener("dragleave", handleColumnDragLeave);
      colEl.addEventListener("drop", handleColumnDrop);

      colEl
        .querySelector(".add-card-btn")
        .addEventListener("click", () => openCreateCard(col.id));
      colEl
        .querySelector(".btn-primary")
        .addEventListener("click", () => openCreateCard(col.id));

      board.appendChild(colEl);
    });
  }

  function createCardElement(card, column) {
    const el = document.createElement("div");
    el.className = "card card-sm bg-base-100 shadow-sm cursor-move border border-base-300";
    el.draggable = true;
    el.dataset.filename = card.filename;
    el.dataset.column = column;

    const categoriesHtml =
      card.categories && card.categories.length && !card.blocked
        ? `<div class="flex flex-wrap gap-1 mt-2">${card.categories.map((c) => `<span class="badge badge-primary badge-sm">${escapeHtml(c)}</span>`).join("")}</div>`
        : "";

    const priorityHtml = card.priority > 0
      ? `<span class="badge badge-sm ${card.priority === 1 ? 'badge-error' : card.priority === 2 ? 'badge-warning' : 'badge-ghost'}">P${card.priority}</span>`
      : "";

    const roleHtml = card.role
      ? `<span class="badge badge-sm badge-outline">${escapeHtml(card.role)}</span>`
      : "";

    const blockedHtml = card.blocked
      ? `<span class="badge badge-sm badge-error">blocked</span>`
      : "";

    el.innerHTML = `
          <div class="card-body p-3 ${card.blocked ? 'opacity-60' : ''}">
              <div class="flex justify-between items-start">
                  <h3 class="card-title text-sm font-medium">${escapeHtml(card.title)}</h3>
                  <div class="card-actions justify-end gap-1">
                      ${blockedHtml}
                      ${roleHtml}
                      ${priorityHtml}
                      <button class="btn btn-ghost btn-xs p-1 min-h-0 h-auto" title="Edit">✏️</button>
                      <button class="btn btn-ghost btn-xs p-1 min-h-0 h-auto text-error" title="Delete">✕</button>
                  </div>
              </div>
              ${categoriesHtml}
              <div class="text-xs opacity-60 mt-2">Updated: ${formatDate(card.updated)}</div>
          </div>
      `;

    el.addEventListener("dragstart", handleDragStart);
    el.addEventListener("dragend", handleDragEnd);
    el.querySelector("[title='Edit']").addEventListener("click", (e) => {
      e.stopPropagation();
      openEditCard(column, card);
    });
    el.querySelector("[title='Delete']").addEventListener("click", (e) => {
      e.stopPropagation();
      deleteCard(column, card.filename);
    });

    return el;
  }

  function handleDragStart(e) {
    draggedCard = this;
    this.classList.add("dragging");
    e.dataTransfer.effectAllowed = "move";
    e.dataTransfer.setData(
      "text/plain",
      JSON.stringify({
        filename: this.dataset.filename,
        fromColumn: this.dataset.column,
      }),
    );
  }

  function handleDragEnd() {
    this.classList.remove("dragging");
    draggedCard = null;
    document
      .querySelectorAll(".column-cards")
      .forEach((c) => c.classList.remove("drag-over"));
  }

  function handleDragOver(e) {
    e.preventDefault();
    if (draggedColumn) return;
    e.dataTransfer.dropEffect = "move";
    this.classList.add("drag-over");
  }

  function handleDragLeave() {
    this.classList.remove("drag-over");
  }

  async function handleDrop(e) {
    e.preventDefault();
    this.classList.remove("drag-over");

    const raw = e.dataTransfer.getData("text/plain");
    const data = JSON.parse(raw);
    if (data.type === "column") return;

    const toColumn = this.dataset.column;

    if (data.fromColumn === toColumn) return;

    localMoveInProgress = true;
    const cardEl = draggedCard;
    try {
      const res = await fetch(`/api/card/${data.fromColumn}/${data.filename}`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ toColumn }),
      });
      if (!res.ok) throw new Error("Failed to move card");
      if (cardEl) {
        cardEl.style.transition = "opacity 0.2s";
        cardEl.style.opacity = "0";
        cardEl.addEventListener(
          "transitionend",
          () => {
            cardEl.remove();
            const dst = document.querySelector(
              `.column-cards[data-column="${toColumn}"]`,
            );
            if (dst) {
              const prioEl = cardEl.querySelector(".badge-sm");
              const priority = prioEl && prioEl.textContent.startsWith("P") ? parseInt(prioEl.textContent.slice(1)) : 0;
              const roleEl = cardEl.querySelector(".badge-outline");
              const role = roleEl ? roleEl.textContent : "";
              const blockedEl = cardEl.querySelector(".badge-error");
              const blocked = blockedEl && blockedEl.textContent === "blocked";
              const newEl = createCardElement(
                {
                  filename: data.filename,
                  title: cardEl.querySelector(".card-title").textContent,
                  updated: new Date().toISOString(),
                  priority: priority,
                  role: role,
                  blocked: blocked,
                },
                toColumn,
              );
              newEl.style.opacity = "0";
              newEl.style.transition = "opacity 0.2s";
              dst.appendChild(newEl);
              requestAnimationFrame(() => {
                requestAnimationFrame(() => {
                  newEl.style.opacity = "1";
                });
              });
            }
            updateColumnCount(data.fromColumn);
            updateColumnCount(toColumn);
            setTimeout(() => {
              localMoveInProgress = false;
            }, 100);
          },
          { once: true },
        );
      }
    } catch (err) {
      console.error(err);
      alert("Failed to move card");
      loadBoard();
      localMoveInProgress = false;
    }
  }

  function handleColumnDragStart(e) {
    e.stopPropagation();
    draggedColumn = this.closest(".column");
    draggedColumn.classList.add("dragging");
    e.dataTransfer.effectAllowed = "move";
    e.dataTransfer.setData(
      "text/plain",
      JSON.stringify({
        type: "column",
        slug: draggedColumn.dataset.slug,
      }),
    );
    setTimeout(() => (draggedColumn.style.opacity = "0.4"), 0);
  }

  function handleColumnDragEnd() {
    if (draggedColumn) {
      draggedColumn.style.opacity = "";
      draggedColumn.classList.remove("dragging");
    }
    draggedColumn = null;
    removeColumnDropIndicators();
  }

  function handleColumnDragOver(e) {
    e.preventDefault();
    if (!draggedColumn) return;
    e.stopPropagation();

    const target = this.closest(".column");
    if (!target || target === draggedColumn) return;

    const rect = this.getBoundingClientRect();
    const midX = rect.left + rect.width / 2;

    removeColumnDropIndicators();

    const indicator = document.createElement("div");
    indicator.className = "column-drop-indicator";

    if (e.clientX < midX) {
      target.parentNode.insertBefore(indicator, target);
    } else {
      target.parentNode.insertBefore(indicator, target.nextSibling);
    }
  }

  function handleColumnDragEnter(e) {
    e.preventDefault();
  }

  function handleColumnDragLeave(e) {
    const target = this.closest(".column");
    const rect = target.getBoundingClientRect();
    if (
      e.clientX < rect.left ||
      e.clientX > rect.right ||
      e.clientY < rect.top ||
      e.clientY > rect.bottom
    ) {
      removeColumnDropIndicators();
    }
  }

  function removeColumnDropIndicators() {
    document
      .querySelectorAll(".column-drop-indicator")
      .forEach((el) => el.remove());
  }

  async function handleColumnDrop(e) {
    e.preventDefault();
    e.stopPropagation();
    removeColumnDropIndicators();

    if (!draggedColumn) return;

    const target = this.closest(".column");
    if (!target || target === draggedColumn) return;

    const data = JSON.parse(e.dataTransfer.getData("text/plain"));
    if (data.type !== "column") return;

    const rect = target.getBoundingClientRect();
    const beforeSlug =
      e.clientX < rect.left + rect.width / 2
        ? target.dataset.slug
        : getNextColumnSlug(target) || "";

    try {
      const res = await fetch(`/api/column/${data.slug}`, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ beforeSlug }),
      });
      if (!res.ok) throw new Error("Failed to reorder column");
      await loadBoard();
    } catch (err) {
      console.error(err);
      alert("Failed to reorder column");
    }
  }

  function getNextColumnSlug(target) {
    const next = target.nextElementSibling;
    return next && next.dataset.slug ? next.dataset.slug : null;
  }

  function openCreateCard(column) {
    document.getElementById("dialog-title").textContent = "New Card";
    document.getElementById("card-column").value = column;
    document.getElementById("card-filename").value = "";
    document.getElementById("card-title-input").value = "";
    document.getElementById("card-content-input").value = "";
    document.getElementById("card-priority-input").value = "";
    document.getElementById("card-role-input").value = "";
    document.getElementById("card-blocked-input").checked = false;
    document.getElementById("card-categories-input").value = "";
    clearCategories();
    currentColumn = column;
    currentFilename = "";
    commentsList.innerHTML = "";
    addCommentBtn.style.display = "none";
    commentInput.style.display = "none";
    showMarkdownPreview();
    cardDialog.showModal();
    document.getElementById("card-title-input").focus();
  }

  async function openEditCard(column, card) {
    currentColumn = column;
    currentFilename = card.filename;
    addCommentBtn.style.display = "";
    commentInput.style.display = "";
    try {
      const res = await fetch(`/api/card/${column}/${card.filename}`);
      if (res.ok) {
        card = await res.json();
      }
    } catch (_) {}
    document.getElementById("dialog-title").textContent = "Edit Card";
    document.getElementById("card-column").value = column;
    document.getElementById("card-filename").value = card.filename;
    document.getElementById("card-title-input").value = card.title;
    document.getElementById("card-content-input").value = card.content;
    document.getElementById("card-priority-input").value = card.priority || "";
    document.getElementById("card-role-input").value = card.role || "";
    document.getElementById("card-blocked-input").checked = !!card.blocked;
    setCategories(card.categories || []);
    renderComments(card.comments);
    showMarkdownPreview();
    cardDialog.showModal();
    document.getElementById("card-title-input").focus();
  }

  cardForm.addEventListener("submit", async (e) => {
    e.preventDefault();
    const column = document.getElementById("card-column").value;
    const filename = document.getElementById("card-filename").value;
    const title = document.getElementById("card-title-input").value;
    const content = document.getElementById("card-content-input").value;
    const categories = getCategories();
    const priority = parseInt(document.getElementById("card-priority-input").value) || 0;
    const role = document.getElementById("card-role-input").value.trim();
    const blocked = document.getElementById("card-blocked-input").checked;

    try {
      let res;
      if (filename) {
        res = await fetch(`/api/card/${column}/${filename}`, {
          method: "PUT",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ title, content, categories, priority, role, blocked }),
        });
      } else {
        res = await fetch("/api/cards", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ column, title, content, categories, priority, role, blocked }),
        });
      }
      if (!res.ok) throw new Error("Failed to save card");
      clearCategories();
      showMarkdownPreview();
      cardDialog.close();
    } catch (err) {
      console.error(err);
      alert("Failed to save card");
    }
  });

  addCommentBtn.addEventListener("click", addComment);
  commentInput.addEventListener("keydown", (e) => {
    if (e.key === "Enter") {
      e.preventDefault();
      addComment();
    }
  });

  document.getElementById("cancel-btn").addEventListener("click", () => {
    cardDialog.close();
    clearCategories();
    showMarkdownPreview();
  });
  cardDialog.addEventListener("click", (e) => {
    if (e.target === cardDialog) {
      cardDialog.close();
      clearCategories();
      showMarkdownPreview();
    }
  });

  async function deleteCard(column, filename) {
    if (!confirm("Delete this card?")) return;
    try {
      const res = await fetch(`/api/card/${column}/${filename}`, {
        method: "DELETE",
      });
      if (!res.ok) throw new Error("Failed to delete card");
    } catch (err) {
      console.error(err);
      alert("Failed to delete card");
    }
  }

  document.getElementById("add-column-btn").addEventListener("click", () => {
    document.getElementById("column-name-input").value = "";
    columnDialog.showModal();
    document.getElementById("column-name-input").focus();
  });

  document
    .getElementById("column-cancel-btn")
    .addEventListener("click", () => columnDialog.close());
  columnDialog.addEventListener("click", (e) => {
    if (e.target === columnDialog) columnDialog.close();
  });

  columnForm.addEventListener("submit", async (e) => {
    e.preventDefault();
    const name = document.getElementById("column-name-input").value.trim();
    if (!name) return;

    const slug = name
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, "-")
      .replace(/^-|-$/g, "");

    try {
      const res = await fetch("/api/columns", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ id: slug, name }),
      });
      if (!res.ok) throw new Error("Failed to create column");
      columnDialog.close();
    } catch (err) {
      console.error(err);
      alert("Failed to create column");
    }
  });

  function renderComments(comments) {
    commentsList.innerHTML = "";
    if (!comments || !comments.length) {
      const empty = document.createElement("p");
      empty.className = "text-xs opacity-50 italic";
      empty.textContent = "No comments yet.";
      commentsList.appendChild(empty);
      return;
    }
    comments.forEach((c) => {
      const el = document.createElement("div");
      el.className = "text-sm bg-base-300 rounded p-2";
      el.innerHTML = `
        <div class="flex justify-between text-xs opacity-60 mb-1">
          <span>${escapeHtml(c.author)}</span>
          <span>${formatDate(c.created)}</span>
        </div>
        <div>${escapeHtml(c.text)}</div>
      `;
      commentsList.appendChild(el);
    });
  }

  async function addComment() {
    const text = commentInput.value.trim();
    if (!text || !currentFilename) return;
    try {
      const res = await fetch(`/api/card/${currentColumn}/${currentFilename}/comment`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ text }),
      });
      if (!res.ok) throw new Error("Failed to add comment");
      commentInput.value = "";
      const cardRes = await fetch(`/api/card/${currentColumn}/${currentFilename}`);
      if (cardRes.ok) {
        const card = await cardRes.json();
        renderComments(card.comments);
      }
    } catch (err) {
      console.error(err);
      alert("Failed to add comment");
    }
  }

  function escapeHtml(text) {
    const div = document.createElement("div");
    div.textContent = text;
    return div.innerHTML;
  }

  function formatDate(dateStr) {
    if (!dateStr) return "";
    const d = new Date(dateStr);
    return d.toLocaleDateString(undefined, {
      month: "short",
      day: "numeric",
      year: "numeric",
    });
  }

  function updateColumnCount(columnId) {
    const colEl = document.querySelector(`.column[data-column="${columnId}"]`);
    if (!colEl) return;
    const cards = colEl.querySelectorAll(".column-cards > .card");
    const countEl = colEl.querySelector(".column-count");
    if (countEl) {
      countEl.textContent = `(${cards.length})`;
    }
  }

  function animateCardMove(fromColumn, toColumn, card) {
    const srcContainer = document.querySelector(
      `.column-cards[data-column="${fromColumn}"]`,
    );
    const dstContainer = document.querySelector(
      `.column-cards[data-column="${toColumn}"]`,
    );
    if (!srcContainer || !dstContainer) {
      loadBoard();
      return;
    }

    const existing = srcContainer.querySelector(
      `.card[data-filename="${card.filename}"]`,
    );
    if (existing) {
      existing.style.transition = "opacity 0.2s";
      existing.style.opacity = "0";
      existing.addEventListener(
        "transitionend",
        () => {
          existing.remove();
          insertCardAnimated(dstContainer, card);
          updateColumnCount(fromColumn);
          updateColumnCount(toColumn);
        },
        { once: true },
      );
    } else {
      insertCardAnimated(dstContainer, card);
      updateColumnCount(fromColumn);
      updateColumnCount(toColumn);
    }
  }

  function animateCardCreated(column, card) {
    const container = document.querySelector(
      `.column-cards[data-column="${column}"]`,
    );
    if (!container) return;
    insertCardAnimated(container, card);
  }

  function animateCardDeleted(column, filename) {
    const container = document.querySelector(
      `.column-cards[data-column="${column}"]`,
    );
    if (!container) return;
    const el = container.querySelector(`.card[data-filename="${filename}"]`);
    if (!el) return;
    el.style.transition = "opacity 0.2s";
    el.style.opacity = "0";
    el.addEventListener("transitionend", () => el.remove(), { once: true });
  }

  function animateCardUpdated(column, card) {
    const container = document.querySelector(
      `.column-cards[data-column="${column}"]`,
    );
    if (!container) return;
    const el = container.querySelector(`.card[data-filename="${card.filename}"]`);
    if (!el) return;
    el.querySelector(".card-title").textContent = card.title;
    const updatedEl = el.querySelector(".text-xs.opacity-60");
    if (updatedEl) updatedEl.textContent = `Updated: ${formatDate(card.updated)}`;

    const actions = el.querySelector(".card-actions");

    let prioBadge = actions.querySelector(".badge-sm:not(.badge-outline)");
    if (card.priority > 0) {
      if (prioBadge) {
        prioBadge.textContent = `P${card.priority}`;
        prioBadge.className = `badge badge-sm ${card.priority === 1 ? 'badge-error' : card.priority === 2 ? 'badge-warning' : 'badge-ghost'}`;
      } else {
        prioBadge = document.createElement("span");
        prioBadge.className = `badge badge-sm ${card.priority === 1 ? 'badge-error' : card.priority === 2 ? 'badge-warning' : 'badge-ghost'}`;
        prioBadge.textContent = `P${card.priority}`;
        actions.insertBefore(prioBadge, actions.firstChild);
      }
    } else if (prioBadge) {
      prioBadge.remove();
    }

    let roleBadge = actions.querySelector(".badge-outline");
    if (card.role) {
      if (roleBadge) {
        roleBadge.textContent = card.role;
      } else {
        roleBadge = document.createElement("span");
        roleBadge.className = "badge badge-sm badge-outline";
        roleBadge.textContent = card.role;
        const ref = actions.querySelector(".badge-sm");
        actions.insertBefore(roleBadge, ref || actions.firstChild);
      }
    } else if (roleBadge) {
      roleBadge.remove();
    }

    let blockedBadge = [...actions.querySelectorAll(".badge-error")].find(b => b.textContent === "blocked");
    if (card.blocked) {
      if (!blockedBadge) {
        blockedBadge = document.createElement("span");
        blockedBadge.className = "badge badge-sm badge-error";
        blockedBadge.textContent = "blocked";
        actions.insertBefore(blockedBadge, actions.firstChild);
      }
      el.querySelector(".card-body").classList.add("opacity-60");
    } else {
      if (blockedBadge) blockedBadge.remove();
      el.querySelector(".card-body").classList.remove("opacity-60");
    }

    el.style.transition = "background 0.3s";
    el.style.background = "var(--surface-hover)";
    setTimeout(() => {
      el.style.background = "";
    }, 400);
  }

  function insertCardAnimated(container, card) {
    const el = createCardElement(card, container.dataset.column);
    el.style.opacity = "0";
    el.style.transition = "opacity 0.2s";
    container.appendChild(el);
    requestAnimationFrame(() => {
      requestAnimationFrame(() => {
        el.style.opacity = "1";
      });
    });
  }

  loadBoard();
}
