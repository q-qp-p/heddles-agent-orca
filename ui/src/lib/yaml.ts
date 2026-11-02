/**
 * toYAML renders a JSON value as block-style YAML. It exists to show a merge
 * patch in the form people paste into a file for `kubectl patch --patch-file`,
 * so it covers what JSON holds (objects, arrays, strings, numbers, booleans,
 * null) and nothing more.
 */
export function toYAML(value: unknown, indent = 0): string {
  const pad = '  '.repeat(indent)
  if (Array.isArray(value)) {
    if (value.length === 0) return `${pad}[]\n`
    return value
      .map((item) => {
        if (isContainer(item) && !isEmpty(item)) {
          // "- " then the first line of the nested block, the rest aligned under it.
          const nested = toYAML(item, indent + 1).split('\n')
          const first = nested[0].trimStart()
          return [`${pad}- ${first}`, ...nested.slice(1)].join('\n').replace(/\n$/, '') + '\n'
        }
        return `${pad}- ${scalar(item)}\n`
      })
      .join('')
  }
  if (value !== null && typeof value === 'object') {
    const entries = Object.entries(value as Record<string, unknown>)
    if (entries.length === 0) return `${pad}{}\n`
    return entries
      .map(([k, v]) => {
        if (isContainer(v) && !isEmpty(v)) return `${pad}${key(k)}:\n${toYAML(v, indent + 1)}`
        return `${pad}${key(k)}: ${scalar(v)}\n`
      })
      .join('')
  }
  return `${pad}${scalar(value)}\n`
}

function isContainer(v: unknown): boolean {
  return v !== null && typeof v === 'object'
}

function isEmpty(v: unknown): boolean {
  return Array.isArray(v) ? v.length === 0 : Object.keys(v as object).length === 0
}

function scalar(v: unknown): string {
  if (v === null || v === undefined) return 'null'
  if (Array.isArray(v)) return '[]'
  if (typeof v === 'object') return '{}'
  if (typeof v === 'string') return quote(v)
  return String(v)
}

/** Keys are quoted only when plain style would change their meaning. */
function key(k: string): string {
  return /^[A-Za-z_][A-Za-z0-9_.\/-]*$/.test(k) && !/^(true|false|null|yes|no|on|off)$/i.test(k) ? k : JSON.stringify(k)
}

/** Strings are quoted when plain style would read as another type or break the line. */
function quote(s: string): string {
  const plain = /^[A-Za-z_][A-Za-z0-9_.\/@-]*$/.test(s) && !/^(true|false|null|yes|no|on|off|~)$/i.test(s)
  return plain ? s : JSON.stringify(s)
}
