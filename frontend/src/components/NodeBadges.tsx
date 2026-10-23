import { cardName, STAGE_LABEL, type Binding } from '../lib/cards'

// 画布节点上的能力徽章。
//
// 排布规则（解决"多张卡叠在一起看不清"）：
//   - 横向排在节点**上方**，最多 2 枚，其余折成 "+N"（3 枚会把 180px 的节点撑太宽，
//     相邻节点的徽章就会叠在一起——"看得见"不等于"看得清"）；
//   - 形状区分类型（圆/六边形/三层），与能力库一致；
//   - 顺序按执行阶段：输入 → 本体 → 输出 → 替换，左到右就是执行顺序；
//   - 整体 pointer-events:none，不挡节点拖拽与连线。

const GLYPH: Record<string, string> = { agent: '', skill: '', workflow: '' }

export default function NodeBadges({ bindings }: { bindings: Binding[] }) {
  if (bindings.length === 0) return null

  const ordered = [...bindings].sort((a, b) => {
    const rank = (s?: string) => ({ input: 0, body: 1, output: 2, replace: 3 } as Record<string, number>)[s || 'body'] ?? 1
    return rank(a.stage) - rank(b.stage)
  })
  const shown = ordered.slice(0, 2)
  const rest = ordered.length - shown.length

  return (
    <div className="fe-node-badges">
      {shown.map((b, i) => (
        <span
          key={`${b.kind}:${b.id}:${i}`}
          className={`fe-node-badge fe-node-badge--${b.kind}`}
          title={`${STAGE_LABEL[b.stage || 'body']} · ${cardName(b.kind, b.id)}`}
        >
          <span className="fe-node-badge__dot">{GLYPH[b.kind]}</span>
          <span className="fe-node-badge__label">{cardName(b.kind, b.id)}</span>
        </span>
      ))}
      {rest > 0 && (
        <span className="fe-node-badge fe-node-badge--more" title={`还有 ${rest} 张卡（在右侧面板查看）`}>
          +{rest}
        </span>
      )}
    </div>
  )
}
