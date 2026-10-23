import { useEffect, useState } from 'react'

// 通用节点参数区：给运行期插件这类**前端不认识的节点类型**兜底。
//
// 为什么不给插件做专属表单：插件是用户自己写的（script/http），前端不可能预知
// 它要哪些字段。硬做一套"猜字段"的表单只会更糟。给它一个结构化 JSON 编辑器，
// 至少让参数可达、可改、可校验——这已经是「能用」的最低线。
//
// 交互刻意做成"显式应用"而不是 onChange 直写：JSON 是中间态会被打断的输入，
// 边打边写会把半个对象存进节点，运行时才发现字段丢了。

interface Props {
  data: Record<string, unknown>
  onUpdate: (key: string, value: unknown) => void
}

export default function GenericNodeFields({ data, onUpdate }: Props) {
  // 下划线开头的键是前端内部标记（_label 等），不进编辑器
  const visible = Object.fromEntries(Object.entries(data).filter(([k]) => !k.startsWith('_')))
  const [text, setText] = useState(() => JSON.stringify(visible, null, 2))
  const [err, setErr] = useState('')
  const [dirty, setDirty] = useState(false)

  useEffect(() => {
    if (!dirty) setText(JSON.stringify(visible, null, 2))
    // visible 每次渲染都是新对象，依赖它会导致输入被反复覆盖；
    // 只在节点切换（dirty 复位）时同步，见调用方的 key
  }, [dirty]) // eslint-disable-line react-hooks/exhaustive-deps

  const apply = () => {
    try {
      const parsed = JSON.parse(text)
      if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
        setErr('必须是一个 JSON 对象')
        return
      }
      setErr('')
      setDirty(false)
      Object.entries(parsed as Record<string, unknown>).forEach(([k, v]) => onUpdate(k, v))
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    }
  }

  return (
    <div className="flex flex-col gap-2">
      <div className="rounded-lg border border-amber-500/30 bg-amber-500/10 px-2 py-1.5">
        <p className="text-[10px] text-amber-300 leading-relaxed">
          这是运行期插件节点：字段由插件自己定义，前端不做专属表单，直接编辑参数 JSON。
          {typeof data._label === 'string' && (
            <span className="ml-1">插件：{String(data._label)}</span>
          )}
        </p>
      </div>
      <textarea
        value={text}
        onChange={(e) => {
          setText(e.target.value)
          setDirty(true)
        }}
        rows={10}
        spellCheck={false}
        className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-[11px] font-mono outline-none focus:border-primary"
      />
      {err && <p className="text-[10px] text-red-300">JSON 解析失败：{err}</p>}
      <div className="flex items-center gap-2">
        <button
          onClick={apply}
          disabled={!dirty}
          className={`px-2 py-1 rounded text-[10px] border ${
            dirty
              ? 'fe-btn-primary border-transparent'
              : 'border-border text-gray-500 cursor-not-allowed'
          }`}
        >
          应用参数
        </button>
        <span className="text-[10px] text-gray-600">改了要点「应用参数」才写入节点</span>
      </div>
    </div>
  )
}
