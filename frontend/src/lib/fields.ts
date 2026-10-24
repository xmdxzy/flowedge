import type { PipelineField } from '../api/types'

// 表单值 → 请求体参数（含校验）。
//
// 三条与后端对齐的纪律（backend/service/pipeline.go coerceField 是同一套语义）：
//  1. 未知字段会被后端**整体拒绝**，所以这里只发流水线声明过的 key；
//  2. 非必填空值不发送，让后端合并 JSON 里的默认值——发 0/"" 等于改配置；
//  3. 必填为空在前端就拦住并说清缺哪个，不让用户点完按钮才看到 400。

export type FieldValue = string | number | boolean | null
export type FormValues = Record<string, FieldValue>

export interface FieldError {
  key: string
  label: string
  message: string
  /** true = 必填为空（与「格式不对」分开，按钮禁用文案不同） */
  missing: boolean
}

/** 字段初值：default 预填；没有 default 时给该类型的空值 */
export function defaultFieldValue(field: PipelineField): FieldValue {
  const d = field.default
  switch (field.type) {
    case 'bool':
      return typeof d === 'boolean' ? d : false
    case 'int':
    case 'float': {
      if (typeof d === 'number' && Number.isFinite(d)) return d
      if (typeof d === 'string' && d.trim() !== '' && Number.isFinite(Number(d))) return Number(d)
      return ''
    }
    case 'enum': {
      const options = field.options ?? []
      const s = typeof d === 'string' ? d : ''
      return options.includes(s) ? s : (options[0] ?? '')
    }
    default:
      if (typeof d === 'string') return d
      return d == null ? '' : String(d)
  }
}

/** 整张表单的初值 */
export function initialForm(fields: PipelineField[]): FormValues {
  const form: FormValues = {}
  for (const f of fields) form[f.key] = defaultFieldValue(f)
  return form
}

function toNumber(raw: FieldValue): number | null {
  if (typeof raw === 'number') return Number.isFinite(raw) ? raw : null
  if (typeof raw !== 'string') return null
  const s = raw.trim()
  if (!s) return null
  const n = Number(s)
  return Number.isFinite(n) ? n : null
}

const err = (
  field: PipelineField,
  message: string,
  missing = false,
): { error: FieldError } => ({ error: { key: field.key, label: field.label, message, missing } })

function checkRange(field: PipelineField, value: number): string | null {
  if (field.min != null && value < field.min) return `「${field.label}」不能小于 ${field.min}`
  if (field.max != null && value > field.max) return `「${field.label}」不能大于 ${field.max}`
  return null
}

/**
 * 单字段：校验 + 转成请求体里的值。
 * `value` 缺失（undefined）表示「这个键不发」，后端会用流水线定义里的默认值。
 */
export function coerceField(
  field: PipelineField,
  raw: FieldValue,
): { value?: unknown; error?: FieldError } {
  // 显式判空而不是调用 helper：这里需要 TypeScript 把 null 从后续分支里收窄掉
  if (raw === null || (typeof raw === 'string' && raw.trim() === '')) {
    if (field.required) return err(field, `「${field.label}」不能为空`, true)
    return {}
  }

  switch (field.type) {
    case 'text':
    case 'textarea':
    case 'path': {
      const s = String(raw).trim()
      if (!s && field.required) return err(field, `「${field.label}」不能为空`, true)
      return s ? { value: s } : {}
    }

    case 'enum': {
      const s = String(raw).trim()
      const options = field.options ?? []
      if (!options.includes(s)) {
        return err(field, `「${field.label}」只能是 ${options.join(' / ')}，收到 "${s}"`)
      }
      return { value: s }
    }

    case 'bool': {
      if (typeof raw === 'boolean') return { value: raw }
      if (typeof raw === 'number') return { value: raw !== 0 }
      const s = raw.trim().toLowerCase()
      if (s === 'true' || s === '1') return { value: true }
      if (s === 'false' || s === '0') return { value: false }
      return err(field, `「${field.label}」要 true/false，收到 "${s}"`)
    }

    case 'int': {
      const n = toNumber(raw)
      if (n === null) return err(field, `「${field.label}」要整数，收到 "${String(raw)}"`)
      // 数字输入框允许打出小数：四舍五入到整数再发，避免后端直接 400
      const rounded = Math.round(n)
      const range = checkRange(field, rounded)
      if (range) return err(field, range)
      return { value: rounded }
    }

    case 'float': {
      const n = toNumber(raw)
      if (n === null) return err(field, `「${field.label}」要数字，收到 "${String(raw)}"`)
      const range = checkRange(field, n)
      if (range) return err(field, range)
      return { value: n }
    }

    default:
      return err(field, `「${field.label}」的类型不支持：${String(field.type)}`)
  }
}

export interface BuildResult {
  params: Record<string, unknown>
  errors: FieldError[]
  /** 必填为空的 label（按钮禁用时直接列出缺什么） */
  missing: string[]
}

/** 整张表单 → {pipeline, params} 里的 params；有任何错误时 params 不完整，调用方不应提交 */
export function buildParams(fields: PipelineField[], form: FormValues): BuildResult {
  const params: Record<string, unknown> = {}
  const errors: FieldError[] = []
  const missing: string[] = []

  for (const field of fields) {
    const raw = field.key in form ? form[field.key] : defaultFieldValue(field)
    const result = coerceField(field, raw)
    if (result.error) {
      errors.push(result.error)
      if (result.error.missing) missing.push(result.error.label)
      continue
    }
    if (result.value !== undefined) params[field.key] = result.value
  }

  return { params, errors, missing }
}

/** 按钮禁用/悬停时的一句话原因 */
export function describeBlockers(errors: FieldError[]): string {
  if (errors.length === 0) return ''
  const missing = errors.filter((e) => e.missing).map((e) => e.label)
  if (missing.length > 0) return `还缺必填：${missing.join('、')}`
  const first = errors[0]
  return errors.length > 1 ? `${first.message}（另有 ${errors.length - 1} 项不合法）` : first.message
}
