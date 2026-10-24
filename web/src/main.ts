// mdedit editor page. Reads the file path from location.pathname, loads it
// from /api/file, edits it with Milkdown, and saves with a conditional PUT.

import { ApiError, fetchFile, saveFile } from "./api";
import { createEditor, type EditorHandle } from "./editor";

const EDIT_PREFIX = "/edit/";
const IMG_PREFIX = "/img/";
const VIEW_BASE_META_NAME = "mdedit-view-base";
const TRANSIENT_STATUS_MS = 1500;
const REFORMAT_NOTE = "Saving will reformat this file's Markdown.";
const TITLE_SUFFIX = " - mdedit";

const STATUS_LOADING = "Loading…";
const STATUS_SAVED = "Saved";
const STATUS_UNSAVED = "Unsaved";
const STATUS_SAVING = "Saving…";
const STATUS_CONFLICT = "Conflict";
const STATUS_NOTHING = "Nothing to save";
const STATUS_RELOADED = "Reloaded";
const LABEL_RAW = "Raw";
const LABEL_WYSIWYG = "Rich";

type Status = "saved" | "unsaved" | "saving" | "conflict" | "error";

interface Elements {
  path: HTMLElement;
  status: HTMLElement;
  note: HTMLElement;
  view: HTMLAnchorElement;
  rawToggle: HTMLButtonElement;
  save: HTMLButtonElement;
  editor: HTMLElement;
  raw: HTMLTextAreaElement;
  conflict: HTMLElement;
  conflictText: HTMLElement;
  reload: HTMLButtonElement;
  error: HTMLElement;
}

function byId<T extends HTMLElement>(id: string): T {
  const el = document.getElementById(id);
  if (el === null) throw new Error(`missing element #${id}`);
  return el as T;
}

function getElements(): Elements {
  return {
    path: byId<HTMLElement>("path"),
    status: byId<HTMLElement>("status"),
    note: byId<HTMLElement>("note"),
    view: byId<HTMLAnchorElement>("view"),
    rawToggle: byId<HTMLButtonElement>("raw-toggle"),
    save: byId<HTMLButtonElement>("save"),
    editor: byId<HTMLElement>("editor"),
    raw: byId<HTMLTextAreaElement>("raw"),
    conflict: byId<HTMLElement>("conflict"),
    conflictText: byId<HTMLElement>("conflict-text"),
    reload: byId<HTMLButtonElement>("reload"),
    error: byId<HTMLElement>("error"),
  };
}

/** The file path as it appears in the URL (percent-encoded) and decoded. */
interface FilePath {
  encoded: string;
  decoded: string;
}

function filePathFromLocation(pathname: string): FilePath | null {
  if (!pathname.startsWith(EDIT_PREFIX)) return null;
  const encoded = pathname.slice(EDIT_PREFIX.length);
  if (encoded === "") return null;
  try {
    return { encoded, decoded: decodeURIComponent(encoded) };
  } catch {
    return null;
  }
}

/** `<base href="/img/<dir>/">` so relative image references resolve to /img/. */
function installImageBase(encodedPath: string): void {
  const slash = encodedPath.lastIndexOf("/");
  const dir = slash === -1 ? "" : encodedPath.slice(0, slash + 1);
  const base = document.createElement("base");
  base.href = `${IMG_PREFIX}${dir}`;
  document.head.prepend(base);
}

function viewBase(): string {
  const meta = document.querySelector<HTMLMetaElement>(`meta[name="${VIEW_BASE_META_NAME}"]`);
  return meta?.content.trim() ?? "";
}

function errorMessage(err: unknown): string {
  if (err instanceof Error) return err.message;
  return String(err);
}

class App {
  private readonly el: Elements;
  private readonly path: FilePath;
  private editor: EditorHandle | null = null;
  private version = "";
  private baseline = "";
  private rawMode = false;
  private saving = false;
  private dirty = false;
  private status: Status = "saved";
  private statusTimer: number | undefined;

  constructor(el: Elements, path: FilePath) {
    this.el = el;
    this.path = path;
  }

  async start(): Promise<void> {
    this.el.path.textContent = this.path.decoded;
    document.title = `${this.path.decoded}${TITLE_SUFFIX}`;
    this.setStatusText(STATUS_LOADING);

    const base = viewBase();
    if (base !== "") {
      this.el.view.href = `${base}/${this.path.encoded}`;
      this.el.view.hidden = false;
    }

    installImageBase(this.path.encoded);

    let file;
    try {
      file = await fetchFile(this.path.decoded);
    } catch (err) {
      this.showError(errorMessage(err));
      return;
    }
    this.version = file.version;

    this.editor = await createEditor({
      root: this.el.editor,
      initialMarkdown: file.content,
      onMarkdownUpdated: (markdown: string) => this.onEditorChanged(markdown),
    });

    this.baseline = this.editor.getMarkdown();
    this.el.note.textContent = this.baseline !== file.content ? REFORMAT_NOTE : "";
    this.setDirty(false);
    this.bindEvents();
    this.el.save.disabled = false;
    this.el.rawToggle.disabled = false;
  }

  private bindEvents(): void {
    this.el.save.addEventListener("click", () => {
      void this.save();
    });
    this.el.rawToggle.addEventListener("click", () => this.toggleRaw());
    this.el.reload.addEventListener("click", () => {
      void this.reload();
    });
    this.el.raw.addEventListener("input", () => {
      this.setDirty(this.el.raw.value !== this.baseline);
    });
    window.addEventListener("keydown", (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && !e.altKey && !e.shiftKey && e.key.toLowerCase() === "s") {
        e.preventDefault();
        void this.save();
      }
    });
    window.addEventListener("beforeunload", (e: BeforeUnloadEvent) => {
      if (this.dirty || this.saving) {
        e.preventDefault();
        e.returnValue = "";
      }
    });
  }

  private onEditorChanged(markdown: string): void {
    if (this.rawMode) return;
    this.setDirty(markdown !== this.baseline);
  }

  /** Current Markdown: textarea verbatim in raw mode, else serialized doc. */
  private currentMarkdown(): string {
    if (this.rawMode) return this.el.raw.value;
    if (this.editor === null) return this.baseline;
    return this.editor.getMarkdown();
  }

  private setDirty(dirty: boolean): void {
    this.dirty = dirty;
    if (this.status === "saving" || this.status === "conflict") return;
    this.setStatus(dirty ? "unsaved" : "saved");
  }

  private setStatus(status: Status, text?: string): void {
    this.status = status;
    const label: Record<Status, string> = {
      saved: STATUS_SAVED,
      unsaved: STATUS_UNSAVED,
      saving: STATUS_SAVING,
      conflict: STATUS_CONFLICT,
      error: text ?? "Error",
    };
    this.setStatusText(label[status]);
    this.el.status.dataset["state"] = status;
  }

  private setStatusText(text: string): void {
    window.clearTimeout(this.statusTimer);
    this.el.status.textContent = text;
  }

  /** Show `text` briefly, then restore the status label. */
  private flashStatus(text: string): void {
    this.setStatusText(text);
    this.statusTimer = window.setTimeout(() => this.setStatus(this.status), TRANSIENT_STATUS_MS);
  }

  private showError(message: string): void {
    this.el.error.textContent = message;
    this.el.error.hidden = false;
    this.setStatus("error", message);
  }

  private clearError(): void {
    this.el.error.textContent = "";
    this.el.error.hidden = true;
  }

  private async save(): Promise<void> {
    if (this.saving || this.editor === null) return;
    const content = this.currentMarkdown();
    const dirty = content !== this.baseline;
    this.dirty = dirty;
    if (!dirty) {
      this.flashStatus(STATUS_NOTHING);
      return;
    }

    this.saving = true;
    this.clearError();
    this.setStatus("saving");
    this.el.save.disabled = true;
    try {
      const resp = await saveFile(this.path.decoded, content, this.version);
      this.version = resp.version;
      this.baseline = content;
      this.el.note.textContent = "";
      this.el.conflict.hidden = true;
      this.status = "saved";
      this.setDirty(this.currentMarkdown() !== this.baseline);
    } catch (err) {
      if (err instanceof ApiError && err.isConflict) {
        this.el.conflictText.textContent = err.message;
        this.el.conflict.hidden = false;
        this.setStatus("conflict");
      } else {
        this.showError(errorMessage(err));
      }
    } finally {
      this.saving = false;
      this.el.save.disabled = false;
    }
  }

  /** Refetch and replace the document, discarding local edits. */
  private async reload(): Promise<void> {
    if (this.saving || this.editor === null) return;
    this.el.reload.disabled = true;
    try {
      const file = await fetchFile(this.path.decoded);
      this.version = file.version;
      this.editor.setMarkdown(file.content, true);
      this.baseline = this.editor.getMarkdown();
      this.el.note.textContent = this.baseline !== file.content ? REFORMAT_NOTE : "";
      if (this.rawMode) this.el.raw.value = this.baseline;
      this.el.conflict.hidden = true;
      this.clearError();
      this.status = "saved";
      this.setDirty(false);
      this.flashStatus(STATUS_RELOADED);
    } catch (err) {
      this.showError(errorMessage(err));
    } finally {
      this.el.reload.disabled = false;
    }
  }

  private toggleRaw(): void {
    if (this.editor === null) return;
    if (!this.rawMode) {
      this.el.raw.value = this.editor.getMarkdown();
      this.rawMode = true;
      this.el.editor.hidden = true;
      this.el.raw.hidden = false;
      this.el.rawToggle.textContent = LABEL_WYSIWYG;
      this.el.rawToggle.setAttribute("aria-pressed", "true");
      this.el.raw.focus();
    } else {
      this.editor.setMarkdown(this.el.raw.value, false);
      this.rawMode = false;
      this.el.raw.hidden = true;
      this.el.editor.hidden = false;
      this.el.rawToggle.textContent = LABEL_RAW;
      this.el.rawToggle.setAttribute("aria-pressed", "false");
      this.setDirty(this.editor.getMarkdown() !== this.baseline);
      this.editor.focus();
    }
  }
}

function main(): void {
  const el = getElements();
  const path = filePathFromLocation(location.pathname);
  if (path === null) {
    el.error.textContent = `Expected a URL like ${EDIT_PREFIX}root/file.md`;
    el.error.hidden = false;
    el.status.textContent = "Error";
    return;
  }
  const app = new App(el, path);
  void app.start();
}

main();
