// Thin wrapper around Milkdown (@milkdown/kit 7.x). Framework-free.

import {
  Editor,
  defaultValueCtx,
  editorViewCtx,
  rootCtx,
} from "@milkdown/kit/core";
import type { Ctx } from "@milkdown/kit/ctx";
import { commonmark, htmlSchema } from "@milkdown/kit/preset/commonmark";
import { gfm } from "@milkdown/kit/preset/gfm";
import { history } from "@milkdown/kit/plugin/history";
import { listener, listenerCtx } from "@milkdown/kit/plugin/listener";
import { $view, getMarkdown, replaceAll } from "@milkdown/kit/utils";
import type { NodeViewConstructor } from "@milkdown/kit/prose/view";

const HTML_NODE_CLASS = "md-html";

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

export interface EditorHandle {
  /** Serialize the current document to Markdown. */
  getMarkdown(): string;
  /** Replace the whole document from Markdown (resets undo history when `flush`). */
  setMarkdown(markdown: string, flush: boolean): void;
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
      ctx.get(listenerCtx).markdownUpdated((_ctx: Ctx, markdown: string) => {
        opts.onMarkdownUpdated(markdown);
      });
    })
    .use(commonmark)
    .use(gfm)
    .use(history)
    .use(listener)
    .use(htmlAsText)
    .create();

  return {
    getMarkdown(): string {
      return editor.action(getMarkdown());
    },
    setMarkdown(markdown: string, flush: boolean): void {
      editor.action(replaceAll(markdown, flush));
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
