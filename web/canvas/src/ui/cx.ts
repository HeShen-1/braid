/** 类名拼接：只为可读性，不引入依赖。 */
export function cx(...parts: Array<string | false | null | undefined>): string {
  let out = ''
  for (const p of parts) {
    if (p === false || p === null || p === undefined || p === '') continue
    out = out === '' ? p : `${out} ${p}`
  }
  return out
}
