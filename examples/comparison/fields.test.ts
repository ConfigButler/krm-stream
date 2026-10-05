// The page's form fields against a real store, without a DOM. Run by `task test-compare`.

import assert from "node:assert/strict";
import { test } from "node:test";
import { LiveResourceStore } from "../../packages/krm-stream/src/index.ts";
import { draftText, editField, type FieldInput, showDraft } from "./fields.ts";

/** An input with a caret, as much of one as the page uses. */
class Input implements FieldInput {
  value = "";
  selectionStart: number | null = 0;
  selectionEnd: number | null = 0;
  readonly dataset: Record<string, string | undefined> = {};
  setSelectionRange(start: number, end: number) {
    this.selectionStart = start;
    this.selectionEnd = end;
  }
  /** Type at the end, as a person whose caret is there would. */
  type(text: string) {
    this.value += text;
    this.selectionStart = this.selectionEnd = this.value.length;
  }
}

const widget = (rv: string, note: string, replicas = 1) => ({
  apiVersion: "e2e.krm-stream.configbutler.io/v1",
  kind: "Widget",
  metadata: { uid: "uid-w", name: "w-00", resourceVersion: rv },
  spec: { note, replicas },
});
const note = ["spec", "note"];

function render(store: LiveResourceStore, input: Input, path = note) {
  showDraft(input, "uid-w", draftText(store, "uid-w", path), true);
}

test("a focused field follows another writer's change, so typing extends the newer value", () => {
  const store = new LiveResourceStore();
  store.applyServerEvent(widget("1", "initial"));
  const input = new Input();
  render(store, input);
  input.setSelectionRange(7, 7); // focused, caret at the end of "initial"

  // Another writer changes the field the person has focused but not edited.
  store.applyServerEvent(widget("2", "external-new-value"));
  render(store, input);
  assert.equal(input.value, "external-new-value");
  assert.deepEqual([input.selectionStart, input.selectionEnd], [7, 7], "the caret stays where it was");

  // The next keystroke extends what the store holds. Before the fix the input still said "initial"
  // and this saved "initial!" at version 2, silently overwriting the other writer.
  input.type("!");
  editField(store, "uid-w", note, input);
  assert.deepEqual(store.captureSave("uid-w"), {
    uid: "uid-w",
    resourceVersion: "2",
    patch: { spec: { note: "external-new-value!" } },
  });
  assert.deepEqual(store.conflicts("uid-w"), []);
});

test("a focused field the person edited keeps their text, and the change becomes a conflict", () => {
  const store = new LiveResourceStore();
  store.applyServerEvent(widget("1", "initial"));
  const input = new Input();
  render(store, input);
  input.type(" mine");
  editField(store, "uid-w", note, input);

  store.applyServerEvent(widget("2", "external-new-value"));
  render(store, input);
  assert.equal(input.value, "initial mine");
  assert.deepEqual(
    store.conflicts("uid-w").map((c) => c.path),
    [note],
  );
});

test("text the store parsed into something else is not replaced while the draft stays put", () => {
  const store = new LiveResourceStore();
  store.applyServerEvent(widget("1", "n", 3));
  const replicas = ["spec", "replicas"];
  const input = new Input();
  render(store, input, replicas);
  assert.equal(input.value, "3");

  // Clearing the field to type another number: the store holds 0, the input stays empty.
  input.value = "";
  editField(store, "uid-w", replicas, input, Number);
  render(store, input, replicas);
  assert.equal(input.value, "");

  // An unrelated server change to another field does not disturb it either.
  store.applyServerEvent(widget("2", "other", 3));
  render(store, input, replicas);
  assert.equal(input.value, "");
});
