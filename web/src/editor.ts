// Thin wrapper around Milkdown (@milkdown/kit 7.x). Framework-free.

import {
  Editor,
  defaultValueCtx,
  editorViewCtx,
  remarkCtx,
  remarkStringifyOptionsCtx,
  rootCtx,
} from "@milkdown/kit/core";
import type { Ctx } from "@milkdown/kit/ctx";
import type { Root } from "mdast";
import { commonmark, htmlSchema, listItemSchema } from "@milkdown/kit/preset/commonmark";
import { gfm } from "@milkdown/kit/preset/gfm";
import { history } from "@milkdown/kit/plugin/history";
import { listener, listenerCtx } from "@milkdown/kit/plugin/listener";
import { $view, getMarkdown, replaceAll } from "@milkdown/kit/utils";
import type { Node as ProseNode } from "@milkdown/kit/prose/model";
import type { NodeViewConstructor, ViewMutationRecord } from "@milkdown/kit/prose/view";
import { EDITOR_STRINGIFY_OPTIONS } from "./stringify-options";

const HTML_NODE_CLASS = "md-html";
const TASK_ITEM_TYPE = "task";
const TASK_CHECKBOX_CLASS = "md-task-checkbox";

/**
 * Node view for the commonmark `html` node. The preset's own toDOM already
 * emits the source as a text node (DOMOutputSpec `['span', attrs, string]`),
 * so raw HTML in Markdown is never live markup. This explicit view pins that
 * invariant so a future preset change cannot silently turn it into markup.
 */
const htmlAsText = $view(htmlSchema.node, (): NodeViewConstructor => {
  return (node) => {
    const dom = document.createElement("span");
    dom.className = HTML_NODE_CLASS;
    dom.dataset["type"] = "html";
    dom.textContent = String(node.attrs["value"] ?? "");
    return { dom };
  };
});

/** Mirror the preset's `li` data attributes so the existing CSS and parseDOM keep working. */
function applyListItemAttrs(li: HTMLLIElement, node: ProseNode): void {
  const checked: boolean | null = (node.attrs["checked"] as boolean | null | undefined) ?? null;
  li.dataset["label"] = String(node.attrs["label"] ?? "");
  li.dataset["listType"] = String(node.attrs["listType"] ?? "");
  li.dataset["spread"] = String(Boolean(node.attrs["spread"]));
  if (checked === null) {
    delete li.dataset["itemType"];
    delete li.dataset["checked"];
  } else {
    li.dataset["itemType"] = TASK_ITEM_TYPE;
    li.dataset["checked"] = String(checked);
  }
}

/**
 * Node view for list items. Plain items render like the preset does. GFM task
 * items (`checked` is a boolean) get a real, clickable checkbox that flips the
 * `checked` attribute through a transaction, so the change reaches the
 * serializer as `[x]` / `[ ]`.
 */
const taskListItem = $view(listItemSchema.node, (): NodeViewConstructor => {
  return (initialNode, view, getPos) => {
    const isTask = (node: ProseNode): boolean => node.attrs["checked"] != null;
    const li = document.createElement("li");
    applyListItemAttrs(li, initialNode);
    if (!isTask(initialNode)) {
      return {
        dom: li,
        contentDOM: li,
        update(node: ProseNode): boolean {
          if (node.type !== initialNode.type || isTask(node)) return false;
          applyListItemAttrs(li, node);
          return true;
        },
      };
    }
    const label = document.createElement("label");
    label.className = TASK_CHECKBOX_CLASS;
    label.contentEditable = "false";
    const checkbox = document.createElement("input");
    checkbox.type = "checkbox";
    checkbox.checked = Boolean(initialNode.attrs["checked"]);
    label.appendChild(checkbox);
    const content = document.createElement("div");
    li.appendChild(label);
    li.appendChild(content);
    checkbox.addEventListener("change", () => {
      const pos = getPos();
      if (pos === undefined) return;
      const node = view.state.doc.nodeAt(pos);
      if (!node) return;
      view.dispatch(
        view.state.tr.setNodeMarkup(pos, undefined, { ...node.attrs, checked: checkbox.checked }),
      );
    });
    return {
      dom: li,
      contentDOM: content,
      update(node: ProseNode): boolean {
        if (node.type !== initialNode.type || !isTask(node)) return false;
        applyListItemAttrs(li, node);
        checkbox.checked = Boolean(node.attrs["checked"]);
        return true;
      },
      stopEvent(event: Event): boolean {
        return event.target === checkbox;
      },
      ignoreMutation(mutation: ViewMutationRecord): boolean {
        return label.contains(mutation.target);
      },
    };
  };
});

export interface EditorHandle {
  /** Serialize the current document to Markdown. */
  getMarkdown(): string;
  /** Replace the whole document from Markdown (resets undo history when `flush`). */
  setMarkdown(markdown: string, flush: boolean): void;
  /**
   * Parse Markdown to mdast exactly as the editor does (remark-parse plus the
   * presets' remark plugins, GFM included), keeping source positions.
   */
  parseMarkdown(markdown: string): Root;
  focus(): void;
  destroy(): Promise<void>;
}

export interface EditorOptions {
  root: HTMLElement;
  initialMarkdown: string;
  /** Called (debounced by Milkdown) with the serialized Markdown after each doc change. */
  onMarkdownUpdated: (markdown: string) => void;
}

export async function createEditor(opts: EditorOptions): Promise<EditorHandle> {
  const editor = await Editor.make()
    .config((ctx: Ctx) => {
      ctx.set(rootCtx, opts.root);
      ctx.set(defaultValueCtx, opts.initialMarkdown);
      // Keep Milkdown's own handlers (marker preservation); only add style options.
      ctx.update(remarkStringifyOptionsCtx, (prev) => ({ ...prev, ...EDITOR_STRINGIFY_OPTIONS }));
      ctx.get(listenerCtx).markdownUpdated((_ctx: Ctx, markdown: string) => {
        opts.onMarkdownUpdated(markdown);
      });
    })
    .use(commonmark)
    .use(gfm)
    .use(history)
    .use(listener)
    .use(htmlAsText)
    .use(taskListItem)
    .create();

  return {
    getMarkdown(): string {
      return editor.action(getMarkdown());
    },
    setMarkdown(markdown: string, flush: boolean): void {
      editor.action(replaceAll(markdown, flush));
    },
    parseMarkdown(markdown: string): Root {
      return editor.action((ctx: Ctx): Root => {
        // Milkdown's ParserState does exactly this (parse, then runSync for the
        // transformer plugins), so the tree matches what the editor loads.
        const remark = ctx.get(remarkCtx);
        return remark.runSync(remark.parse(markdown), markdown) as Root;
      });
    },
    focus(): void {
      editor.action((ctx: Ctx) => {
        ctx.get(editorViewCtx).focus();
      });
    },
    async destroy(): Promise<void> {
      await editor.destroy();
    },
  };
}
