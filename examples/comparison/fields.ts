// How the page's form fields follow the store's draft. Kept apart from page.ts so a node test can run
// it against a real LiveResourceStore with no DOM.
//
// The draft, not the input, is what Save captures. So an input must show the draft whenever the
// draft moves under it — a watch event merged into a field the person has not edited moves it, even
// while that field has focus. Skipping a focused input leaves stale text on screen, and the next
// keystroke writes that stale text plus the key over the newer server value: a silent overwrite with
// no conflict, because the store never saw the person edit the old value. A focused input is
// therefore updated too, keeping its caret.
//
// What must NOT be overwritten is text the store could not take: a half-typed number the store holds
// as something else. So an input is rewritten only when the draft has moved since the input last
// showed it or wrote it, never merely because the two differ.

import type { LiveResourceStore, Path } from "../../packages/krm-stream/src/index.ts";

/** The part of an HTMLInputElement this needs. */
export interface FieldInput {
  value: string;
  readonly selectionStart: number | null;
  readonly selectionEnd: number | null;
  setSelectionRange(start: number, end: number): void;
  readonly dataset: Record<string, string | undefined>;
}

/** The draft's value at path, as an input shows it. */
export function draftText(store: LiveResourceStore, uid: string, path: Path): string {
  let value: unknown = store.draft(uid);
  for (const segment of path) value = (value as Record<string | number, unknown> | undefined)?.[segment];
  return value === undefined || value === null ? "" : String(value);
}

/** What an input last showed or wrote: the object's UID and the text, so selecting another object
 * counts as the draft moving. */
const shown = (uid: string, text: string) => `${uid}\n${text}`;

/** Show uid's draft text in input if it moved since the input last showed or wrote it. A focused
 * input keeps its caret, clamped to the new text. */
export function showDraft(input: FieldInput, uid: string, text: string, focused: boolean): void {
  if (input.dataset.shown === shown(uid, text)) return;
  input.dataset.shown = shown(uid, text);
  if (input.value === text) return;
  const { selectionStart: start, selectionEnd: end } = input;
  input.value = text;
  if (focused && start !== null && end !== null) {
    input.setSelectionRange(Math.min(start, text.length), Math.min(end, text.length));
  }
}

/** The person typed in input: write it to the draft, and remember what the draft now holds, so the
 * next render does not replace text the store parsed into something else. */
export function editField(
  store: LiveResourceStore,
  uid: string,
  path: Path,
  input: FieldInput,
  parse: (text: string) => unknown = (text) => text,
): void {
  store.setValue(uid, path, parse(input.value));
  input.dataset.shown = shown(uid, draftText(store, uid, path));
}
