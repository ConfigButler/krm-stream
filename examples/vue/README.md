# Vue adapter example

Copy `useLiveResource.ts` into a Vue 3 host and change its library import to
`@configbutler/krm-stream`. Call it in `setup()` or an active effect scope. See
[Vue integration](../../docs/vue.md) for ownership and editing guidance.

Run `task test-vue` from the repository root. This typechecks the source and tests its reactive
resource/connection state and subscription disposal. CI runs the same task. Vue is a dependency of
this private example only; the core package and bundle remain framework-independent.
