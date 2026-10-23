import { THEME_OPTIONS, type ThemePref } from '../lib/theme'

// 三段式主题开关：浅色 / 深色 / 跟随系统。
// 为什么不是「一个按钮来回切」：
//   两态按钮无法表达「跟随系统」，用户每次切到系统浅色后，系统一变又被打回固定值；
//   三态分段控件把当前选择显式摆出来，代价只有 78px 宽度。
export default function ThemeSwitch({
  pref,
  onChange,
}: {
  pref: ThemePref
  onChange: (p: ThemePref) => void
}) {
  return (
    <div className="fe-seg" role="group" aria-label="外观主题">
      {THEME_OPTIONS.map((o) => (
        <button
          key={o.value}
          type="button"
          className="fe-seg__item"
          aria-pressed={pref === o.value}
          aria-label={o.label}
          title={`${o.label} · ${o.hint}`}
          onClick={() => onChange(o.value)}
        >
          <span aria-hidden>{o.icon}</span>
        </button>
      ))}
    </div>
  )
}
