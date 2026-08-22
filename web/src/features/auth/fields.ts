/**
 * The one danger treatment for a field that failed its own check, layered onto
 * `inputClass`.
 *
 * Driven off `aria-invalid` rather than a conditional class, so the
 * assistive-tech signal and the visible one cannot come apart: a field is red
 * exactly when it is announced as invalid. That is also how the shadcn inputs on
 * the account screen already behave — they carry their own `aria-invalid:`
 * variants — so the two halves of the product say "this is wrong" the same way.
 */
export const invalidFieldClass =
  'aria-invalid:border-danger aria-invalid:ring-2 aria-invalid:ring-danger/20 ' +
  'aria-invalid:focus:border-danger aria-invalid:focus:ring-danger/20'
